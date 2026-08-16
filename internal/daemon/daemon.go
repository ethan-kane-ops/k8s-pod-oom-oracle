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

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
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
	// Cgroup re-reads the affected cgroup when a report is built. Optional but
	// wanted: see buildReport for why sampled history alone understates the
	// peak.
	Cgroup *cgroup.FS
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
	cgroup    *cgroup.FS
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
	// unattributed counts the subset of those that were inside the kubepods
	// tree, which is this daemon failing rather than the node being noisy.
	unattributed atomic.Uint64
	// watching records that the detector attached successfully.
	watching atomic.Bool
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
		cgroup:    opts.Cgroup,
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
	d.watching.Store(true)
	defer d.watching.Store(false)

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
		// A kill the daemon cannot place is routine outside the kubepods tree
		// and a defect inside it: the report someone is waiting for has just
		// been thrown away. Only the second deserves to be seen at default log
		// level, and only the second belongs in a counter an operator watches.
		if correlate.InKubepodsTree(event.CgroupPath) {
			d.unattributed.Add(1)
			d.log.Warn("dropping an OOM kill inside the kubepods tree",
				"cgroup", event.CgroupPath,
				"effect", "this kill will not appear in any report")
			return
		}
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

	d.applyLiveStats(&report, event)
	d.applyGroupKill(&report, event)

	if d.proc != nil {
		procs, err := d.proc.ProcessesInCgroup(event.CgroupPath)
		if err != nil {
			d.log.Debug("listing container processes", "cgroup", event.CgroupPath, "error", err)
		} else {
			report.Processes = oom.ProcessesFrom(procs, event.Victim)
		}
	}

	return report, true
}

// applyGroupKill records whether the cgroup is killed as an indivisible unit,
// which decides what the process listing means.
//
// A false result means "not observed", not "the container survives". Under group
// kill the container is already gone, so its cgroup is often torn down before
// this read happens and the answer is simply unavailable. Nothing downstream may
// therefore read false as a promise that anything survived, which is why the
// renderer states survival only when this is known to be false and never as the
// default.
func (d *Daemon) applyGroupKill(report *oom.Report, event detector.KillEvent) {
	if d.cgroup == nil {
		return
	}
	groupKill, err := d.cgroup.ReadOOMGroup(event.CgroupPath)
	if err != nil {
		d.log.Debug("reading memory.oom.group",
			"cgroup", event.CgroupPath, "error", err)
		return
	}
	report.GroupKill = groupKill
}

// applyLiveStats corrects the headline numbers using the cgroup itself.
//
// Sampled history is up to one interval stale, and a container can go from idle
// to its limit inside that window: the run that motivated this reported a 4.4MiB
// peak for a container the kernel killed at 512MiB, because the last sample
// predated the whole balloon. memory.peak is monotonic, so re-reading it here
// recovers the true high-water mark even though memory.current has already
// collapsed.
//
// When the container was killed outright its cgroup is already gone and the
// read fails. The victim's own resident memory is then used as a floor, since
// the container cannot have peaked below what one of its processes was holding.
func (d *Daemon) applyLiveStats(report *oom.Report, event detector.KillEvent) {
	if d.cgroup != nil {
		if stats, err := d.cgroup.ReadMemoryStats(event.CgroupPath); err == nil {
			if stats.Peak > report.PeakBytes {
				report.PeakBytes = stats.Peak
			}
			if report.LimitBytes == 0 && stats.Limit != unlimitedLimit {
				report.LimitBytes = stats.Limit
			}
		} else {
			d.log.Debug("re-reading cgroup at report time",
				"cgroup", event.CgroupPath, "error", err)
		}
	}

	if report.PeakBytes < event.Victim.RSSBytes {
		report.PeakBytes = event.Victim.RSSBytes
	}
}

// unlimitedLimit mirrors cgroup.Unlimited.
const unlimitedLimit uint64 = 1<<64 - 1

// nextID builds a report ID that sorts chronologically and stays unique within
// a daemon lifetime.
func (d *Daemon) nextID(at time.Time) string {
	return at.UTC().Format("20060102T150405Z") + "-" + strconv.FormatUint(d.sequence.Add(1), 10)
}

// Ready reports whether the daemon is actually watching this node.
//
// Both halves matter. The detector being attached means a kill will be seen;
// the sampler holding at least one cgroup means the report will have a memory
// trajectory rather than a bare kill notice. A readiness probe that passed
// before both were true would let traffic and, more to the point, test
// workloads start against a daemon that silently misses the first kill.
func (d *Daemon) Ready() bool {
	return d.watching.Load() && len(d.sampler.Tracked()) > 0
}

// Tracked reports how many cgroups the sampler is holding history for.
func (d *Daemon) Tracked() int { return len(d.sampler.Tracked()) }

// Detector names the detection method in use.
func (d *Daemon) Detector() string { return string(d.detector.Source()) }

// Processed reports how many reports have been stored.
func (d *Daemon) Processed() uint64 { return d.processed.Load() }

// Skipped reports how many events were discarded as non-Kubernetes.
func (d *Daemon) Skipped() uint64 { return d.skipped.Load() }

// Unattributed reports the subset of Skipped that came from inside the kubepods
// tree.
//
// It is the half of Skipped worth alerting on. A skip out on the host is the
// daemon doing its job, since the probes see every kill on the node; a skip
// inside the tree is a Kubernetes OOM kill this daemon could not place and
// therefore never reported. Without the split the second is invisible inside
// the first, which is the exact shape of the bug that motivated the counter.
func (d *Daemon) Unattributed() uint64 { return d.unattributed.Load() }
