// Package daemon joins detection, sampling, and correlation into reports.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

// Options configures a Daemon.
type Options struct {
	// Detector observes kills. Required.
	Detector detector.Detector
	// Sampler supplies the memory history behind each report. Required.
	Sampler *sampler.Sampler
	// Store retains finished reports. Required.
	Store store.Store
	// Resolver maps cgroup paths to Kubernetes identities. Required.
	Resolver *correlate.Resolver
	// Proc lists surviving processes for the hog listing. Optional.
	Proc *procfs.FS
	// OnReport is called for each finished report, before it is stored. The
	// daemon uses it to emit Kubernetes Events and Prometheus metrics.
	OnReport func(context.Context, oom.Report)
	// IncludeNonKubernetes keeps kills from cgroups outside the kubepods tree.
	// Off by default: the probes see every kill on the node, and attributing a
	// host service crash to a pod is worse than missing it.
	IncludeNonKubernetes bool
	// Logger receives operational messages.
	Logger *slog.Logger
}

// Daemon consumes kill events and produces post-mortem reports.
type Daemon struct {
	detector  detector.Detector
	sampler   *sampler.Sampler
	store     store.Store
	resolver  *correlate.Resolver
	proc      *procfs.FS
	onReport  func(context.Context, oom.Report)
	includeNK bool
	log       *slog.Logger

	// sequence numbers reports within this daemon's lifetime.
	sequence atomic.Uint64
	// processed counts reports produced, for tests and readiness.
	processed atomic.Uint64
	// skipped counts events discarded as non-Kubernetes.
	skipped atomic.Uint64
}

// New builds a Daemon.
func New(opts Options) (*Daemon, error) {
	switch {
	case opts.Detector == nil:
		return nil, errors.New("daemon requires a detector")
	case opts.Sampler == nil:
		return nil, errors.New("daemon requires a sampler")
	case opts.Store == nil:
		return nil, errors.New("daemon requires a store")
	case opts.Resolver == nil:
		return nil, errors.New("daemon requires a resolver")
	}

	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	return &Daemon{
		detector:  opts.Detector,
		sampler:   opts.Sampler,
		store:     opts.Store,
		resolver:  opts.Resolver,
		proc:      opts.Proc,
		onReport:  opts.OnReport,
		includeNK: opts.IncludeNonKubernetes,
		log:       opts.Logger,
	}, nil
}

// Run starts the sampler and detector and consumes events until the context is
// cancelled. It returns nil on cancellation.
func (d *Daemon) Run(ctx context.Context) error {
	events, err := d.detector.Start(ctx)
	if err != nil {
		return fmt.Errorf("starting detector: %w", err)
	}

	samplerDone := make(chan error, 1)
	go func() { samplerDone <- d.sampler.Run(ctx) }()

	d.log.Info("oom-oracle daemon started", "detector", d.detector.Source())

	for {
		select {
		case <-ctx.Done():
			return <-samplerDone
		case err := <-samplerDone:
			// The sampler failing is fatal: without history every report would
			// be a bare kill notice with no trajectory, which is the product.
			if err != nil {
				return fmt.Errorf("sampler stopped: %w", err)
			}
			return nil
		case event, ok := <-events:
			if !ok {
				return <-samplerDone
			}
			d.Handle(ctx, event)
		}
	}
}

// Handle turns one kill event into a report. It is exported so tests and the
// e2e suite can drive the pipeline directly.
func (d *Daemon) Handle(ctx context.Context, event detector.KillEvent) {
	report, ok := d.buildReport(event)
	if !ok {
		d.skipped.Add(1)
		d.log.Debug("skipping kill outside the kubepods tree", "cgroup", event.CgroupPath)
		return
	}

	if d.onReport != nil {
		d.onReport(ctx, report)
	}

	if err := d.store.Put(ctx, &report); err != nil {
		d.log.Error("storing report", "id", report.ID, "error", err)
		return
	}
	d.processed.Add(1)

	d.log.Info("OOM kill recorded",
		"pod", report.Identity.String(),
		"victim", report.Victim.Comm,
		"pid", report.Victim.PID,
		"inferred", report.Victim.Inferred,
		"limit", report.LimitBytes,
	)
}

// buildReport assembles a report, reporting false when the event should be
// discarded as not belonging to a Kubernetes container.
func (d *Daemon) buildReport(event detector.KillEvent) (oom.Report, bool) {
	identity, resolved := d.resolver.Resolve(event.CgroupPath)
	if !resolved && !d.includeNK {
		return oom.Report{}, false
	}
	if !resolved {
		// The path is not a Kubernetes container, so nothing was parsed from it.
		// Carry it anyway: without it the report identifies nothing at all.
		identity.CgroupPath = event.CgroupPath
	}

	report := oom.Report{
		ID:        d.nextID(event.Time),
		Time:      event.Time,
		Identity:  identity,
		Victim:    event.Victim,
		Source:    event.Source,
		KillCount: event.KillCount,
	}

	// The trajectory is the whole point of having been watching. It is absent
	// only when the container appeared and died inside a single sample window.
	if samples, ok := d.sampler.History(event.CgroupPath); ok && len(samples) > 0 {
		report.Trajectory = oom.TrajectoryFrom(samples)
		report.Trend = sampler.Analyse(samples)

		latest := samples[len(samples)-1]
		report.PeakBytes = latest.Stats.Peak
		if latest.Stats.Limit != unlimitedLimit {
			report.LimitBytes = latest.Stats.Limit
		}
	}

	if d.proc != nil {
		survivors, err := d.proc.ProcessesInCgroup(event.CgroupPath)
		if err != nil {
			d.log.Debug("listing survivors", "cgroup", event.CgroupPath, "error", err)
		} else {
			report.Hogs = oom.HogsFrom(survivors, event.Victim.PID)
		}
	}

	return report, true
}

// unlimitedLimit mirrors cgroup.Unlimited.
const unlimitedLimit uint64 = 1<<64 - 1

// nextID builds a report ID that sorts chronologically and stays unique within
// a daemon lifetime.
func (d *Daemon) nextID(at time.Time) string {
	return at.UTC().Format("20060102T150405Z") + "-" + strconv.FormatUint(d.sequence.Add(1), 10)
}

// Processed reports how many reports have been stored.
func (d *Daemon) Processed() uint64 { return d.processed.Load() }

// Skipped reports how many events were discarded as non-Kubernetes.
func (d *Daemon) Skipped() uint64 { return d.skipped.Load() }
