package detector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

// DefaultPollInterval is deliberately short. The poller identifies the victim
// by diffing process snapshots, so the gap between passes bounds how much of
// the container's process list can turn over unobserved.
const DefaultPollInterval = 500 * time.Millisecond

// defaultEventBuffer sizes the event channel so a slow consumer does not stall
// a polling pass.
const defaultEventBuffer = 64

// PollerOptions configures a Poller.
type PollerOptions struct {
	// Cgroup is the hierarchy to watch. Required.
	Cgroup *cgroup.FS
	// Proc reads process state for victim inference. Optional: without it,
	// kills are still reported but carry no victim.
	Proc *procfs.FS
	// Prefix limits watching to a subtree, typically the kubepods root.
	Prefix string
	// Interval is the gap between polling passes.
	Interval time.Duration
	// Now supplies the current time. Tests inject a deterministic clock.
	Now func() time.Time
	// Logger receives per-cgroup read failures.
	Logger *slog.Logger
}

// Poller detects OOM kills by watching the oom_kill counter in each cgroup's
// memory.events file.
//
// The kernel maintains this counter regardless of tracing support, so the
// poller works on any cgroup v2 host with no BTF, no eBPF, and no elevated
// capabilities beyond reading cgroupfs. It is the fallback that lets the daemon
// stay useful on kernels the eBPF detector cannot attach to.
//
// What it cannot do is name the victim directly. See inferVictim.
type Poller struct {
	cgroup   *cgroup.FS
	proc     *procfs.FS
	prefix   string
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	events    chan KillEvent
	closeOnce sync.Once

	mu sync.Mutex
	// counters is the last observed oom_kill value per cgroup.
	counters map[string]uint64
	// snapshots is the last observed process list per cgroup, keyed by PID.
	// It is what makes victim inference possible: once a process is killed it
	// is gone from /proc, so it can only be identified from an earlier reading.
	snapshots map[string]map[int]procfs.Process
}

// Compile-time check that Poller satisfies the interface.
var _ Detector = (*Poller)(nil)

// NewPoller builds a polling detector.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Cgroup == nil {
		return nil, errors.New("poller requires a cgroup filesystem")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	return &Poller{
		cgroup:    opts.Cgroup,
		proc:      opts.Proc,
		prefix:    opts.Prefix,
		interval:  opts.Interval,
		now:       opts.Now,
		log:       opts.Logger,
		events:    make(chan KillEvent, defaultEventBuffer),
		counters:  make(map[string]uint64),
		snapshots: make(map[string]map[int]procfs.Process),
	}, nil
}

// Source satisfies Detector.
func (p *Poller) Source() Source { return SourcePoller }

// Start primes the baseline, begins polling on the configured interval, and
// returns the event stream.
//
// Callers that want to drive detection themselves, rather than on a ticker,
// use Prime, Poll, and Events instead. The daemon uses Start; tests and the
// e2e suite step manually so they need no sleeps.
func (p *Poller) Start(ctx context.Context) (<-chan KillEvent, error) {
	if err := p.Prime(); err != nil {
		return nil, err
	}

	go p.loop(ctx)

	return p.events, nil
}

// Events returns the event stream. It is valid before Start, so a caller
// driving Poll directly still has somewhere to read from.
func (p *Poller) Events() <-chan KillEvent { return p.events }

// Close stops publishing. Safe to call more than once.
func (p *Poller) Close() error {
	p.closeOnce.Do(func() { close(p.events) })
	return nil
}

func (p *Poller) loop(ctx context.Context) {
	defer p.Close()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.Poll(ctx); err != nil {
				p.log.Error("polling for OOM kills", "error", err)
				return
			}
		}
	}
}

// Prime records the current counters and process snapshots without emitting
// events, establishing the baseline every later pass is compared against.
//
// Without it, every cgroup that had ever been OOM-killed would report as a
// fresh kill the moment the daemon started, so a restart would flood the
// cluster with alerts about history.
func (p *Poller) Prime() error {
	paths, err := p.cgroup.Discover(p.prefix)
	if err != nil {
		return fmt.Errorf("discovering cgroups: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for _, path := range paths {
		stats, err := p.cgroup.ReadMemoryStats(path)
		if err != nil {
			continue
		}
		p.counters[path] = stats.Events.OOMKill
	}
	p.refreshSnapshotsLocked(paths)

	return nil
}

// Poll runs a single detection pass. It is exported so tests and the daemon can
// drive detection deterministically rather than waiting on a ticker.
func (p *Poller) Poll(ctx context.Context) error {
	paths, err := p.cgroup.Discover(p.prefix)
	if err != nil {
		return fmt.Errorf("discovering cgroups: %w", err)
	}

	p.mu.Lock()
	events := p.detectLocked(paths)
	// Refresh snapshots after detection so this pass compares against the
	// previous one, not against itself.
	p.refreshSnapshotsLocked(paths)
	p.evictLocked(paths)
	p.mu.Unlock()

	for _, event := range events {
		select {
		case <-ctx.Done():
			return nil
		case p.events <- event:
		}
	}

	return nil
}

// detectLocked compares current counters against the last pass and builds an
// event for every cgroup whose kill count rose. The caller holds p.mu.
func (p *Poller) detectLocked(paths []string) []KillEvent {
	now := p.now()
	var events []KillEvent

	for _, path := range paths {
		stats, err := p.cgroup.ReadMemoryStats(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				p.log.Debug("reading memory stats", "cgroup", path, "error", err)
			}
			continue
		}

		current := stats.Events.OOMKill
		previous, seen := p.counters[path]
		p.counters[path] = current

		// A cgroup first seen mid-run gets a baseline, not an event: its
		// counter may be non-zero from before the daemon started.
		if !seen || current <= previous {
			continue
		}

		events = append(events, KillEvent{
			Time:       now,
			CgroupPath: path,
			Victim:     p.inferVictimLocked(path),
			KillCount:  current,
			Source:     SourcePoller,
		})
	}

	return events
}

// inferVictimLocked deduces which process died.
//
// The counter says a kill happened but not to whom, and by the time the poller
// notices, the victim is already gone from /proc. The only evidence left is the
// previous snapshot: whichever process was there before and is not there now.
//
// Where several processes vanished in the same interval, the one holding the
// most memory is chosen, because that is who the kernel's badness heuristic
// targets. That is a guess, and the returned victim is marked Inferred so a
// report can say so rather than presenting it as fact. Naming the victim
// exactly is what the eBPF detector is for.
func (p *Poller) inferVictimLocked(path string) Victim {
	previous, ok := p.snapshots[path]
	if !ok || p.proc == nil {
		return Victim{}
	}

	survivors, err := p.proc.ProcessesInCgroup(path)
	if err != nil {
		p.log.Debug("listing survivors", "cgroup", path, "error", err)
		return Victim{}
	}

	alive := make(map[int]struct{}, len(survivors))
	for _, proc := range survivors {
		alive[proc.PID] = struct{}{}
	}

	var candidates []procfs.Process
	for pid, proc := range previous {
		if _, stillAlive := alive[pid]; !stillAlive {
			candidates = append(candidates, proc)
		}
	}
	if len(candidates) == 0 {
		return Victim{}
	}

	// Heaviest first, PID ascending as a stable tie-break.
	slices.SortFunc(candidates, func(a, b procfs.Process) int {
		if a.RSSBytes != b.RSSBytes {
			if a.RSSBytes > b.RSSBytes {
				return -1
			}
			return 1
		}
		return a.PID - b.PID
	})

	return victimFromProcess(candidates[0])
}

// refreshSnapshotsLocked records the current process list for each cgroup.
func (p *Poller) refreshSnapshotsLocked(paths []string) {
	if p.proc == nil {
		return
	}

	for _, path := range paths {
		procs, err := p.proc.ProcessesInCgroup(path)
		if err != nil {
			p.log.Debug("snapshotting processes", "cgroup", path, "error", err)
			continue
		}
		byPID := make(map[int]procfs.Process, len(procs))
		for _, proc := range procs {
			byPID[proc.PID] = proc
		}
		p.snapshots[path] = byPID
	}
}

// evictLocked drops state for cgroups that no longer exist.
func (p *Poller) evictLocked(paths []string) {
	live := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		live[path] = struct{}{}
	}

	for path := range p.counters {
		if _, ok := live[path]; !ok {
			delete(p.counters, path)
			delete(p.snapshots, path)
		}
	}
}
