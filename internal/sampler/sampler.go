package sampler

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

// Defaults sized so the buffered history covers the last minute, matching the
// window the post-mortem renders.
const (
	DefaultInterval    = time.Second
	DefaultHistorySize = 60
	// DefaultRetention is how long a vanished cgroup's history is kept.
	//
	// A container's cgroup disappears the instant it dies, which is exactly when
	// its trajectory becomes interesting. Evicting on the first pass that misses
	// it destroys the history before the detector's event has been handled, so
	// the post-mortem arrives with no trajectory at all.
	DefaultRetention = 30 * time.Second
)

// Options configures a Sampler.
type Options struct {
	// FS is the cgroup hierarchy to sample. Required.
	FS *cgroup.FS
	// Prefix limits sampling to a subtree, typically the kubepods root. An
	// empty prefix samples the whole hierarchy.
	Prefix string
	// Interval is the gap between collection passes.
	Interval time.Duration
	// HistorySize is how many samples each cgroup retains.
	HistorySize int
	// Retention is how long history survives after a cgroup disappears.
	Retention time.Duration
	// Now supplies the current time. Tests inject a deterministic clock.
	Now func() time.Time
	// Logger receives per-cgroup read failures, which are expected churn.
	Logger *slog.Logger
}

// Sampler polls a cgroup hierarchy and retains a rolling history per cgroup.
type Sampler struct {
	fs          *cgroup.FS
	prefix      string
	interval    time.Duration
	historySize int
	retention   time.Duration
	now         func() time.Time
	log         *slog.Logger

	mu    sync.RWMutex
	rings map[string]*Ring
	// vanishedAt records when a cgroup stopped being discovered, so its history
	// can outlive it for long enough to be reported on.
	vanishedAt map[string]time.Time
}

// New builds a Sampler, applying defaults for unset options.
func New(opts Options) (*Sampler, error) {
	if opts.FS == nil {
		return nil, errors.New("sampler requires a cgroup filesystem")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.HistorySize <= 0 {
		opts.HistorySize = DefaultHistorySize
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	return &Sampler{
		fs:          opts.FS,
		prefix:      opts.Prefix,
		interval:    opts.Interval,
		historySize: opts.HistorySize,
		retention:   opts.Retention,
		now:         opts.Now,
		log:         opts.Logger,
		rings:       make(map[string]*Ring),
		vanishedAt:  make(map[string]time.Time),
	}, nil
}

// Run samples on a fixed interval until the context is cancelled. It returns
// nil on cancellation, since a cancelled context is an orderly shutdown.
func (s *Sampler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Collect once up front so a kill in the first interval still has history.
	if err := s.Collect(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Collect(); err != nil {
				return err
			}
		}
	}
}

// Collect runs a single sampling pass over the hierarchy.
//
// Individual cgroup read failures are logged and skipped: on a live node
// cgroups are torn down constantly, and one vanishing container must not stop
// the daemon watching every other container on the node.
func (s *Sampler) Collect() error {
	paths, err := s.fs.Discover(s.prefix)
	if err != nil {
		return fmt.Errorf("discovering cgroups: %w", err)
	}

	now := s.now()
	live := make(map[string]struct{}, len(paths))

	for _, path := range paths {
		stats, err := s.fs.ReadMemoryStats(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.log.Debug("reading memory stats", "cgroup", path, "error", err)
			}
			continue
		}

		// PSI is absent on v1 and on kernels built without CONFIG_PSI. That is
		// a supported configuration, not a failure.
		psi, err := s.fs.ReadPSI(path)
		if err != nil && !errors.Is(err, cgroup.ErrPSIUnsupported) {
			s.log.Debug("reading pressure", "cgroup", path, "error", err)
		}

		live[path] = struct{}{}
		s.record(path, Sample{Time: now, Stats: stats, PSI: psi})
	}

	s.evict(live, now)
	return nil
}

// record appends a sample, allocating a ring for newly seen cgroups.
func (s *Sampler) record(path string, sample Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ring, ok := s.rings[path]
	if !ok {
		// New has already normalised historySize to a positive value.
		ring = NewRing(s.historySize)
		s.rings[path] = ring
	}
	ring.Add(sample)
}

// evict drops history for cgroups that have been gone longer than the retention
// window, so a churning node does not grow the map without bound.
//
// Eviction is deliberately delayed. A container's cgroup vanishes the moment it
// dies, which is precisely when its trajectory matters, so dropping it on the
// first missed pass would race the detector and leave every post-mortem without
// a memory history.
func (s *Sampler) evict(live map[string]struct{}, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for path := range s.rings {
		if _, ok := live[path]; ok {
			// Still present: clear any pending eviction, since a cgroup can
			// briefly disappear from a walk and come back.
			delete(s.vanishedAt, path)
			continue
		}

		firstMissed, seen := s.vanishedAt[path]
		if !seen {
			s.vanishedAt[path] = now
			continue
		}
		if now.Sub(firstMissed) >= s.retention {
			delete(s.rings, path)
			delete(s.vanishedAt, path)
		}
	}
}

// History returns the buffered samples for a cgroup, oldest first. It reports
// false when the cgroup is not being tracked.
func (s *Sampler) History(path string) ([]Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ring, ok := s.rings[path]
	if !ok {
		return nil, false
	}
	return ring.Samples(), true
}

// Latest returns the most recent sample for a cgroup.
func (s *Sampler) Latest(path string) (Sample, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ring, ok := s.rings[path]
	if !ok {
		return Sample{}, false
	}
	return ring.Latest()
}

// Trend fits a growth rate over a cgroup's buffered history.
func (s *Sampler) Trend(path string) (Trend, bool) {
	samples, ok := s.History(path)
	if !ok {
		return Trend{}, false
	}
	return Analyse(samples), true
}

// Tracked lists the cgroups currently held in memory, sorted for stable output.
func (s *Sampler) Tracked() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return slices.Sorted(maps.Keys(s.rings))
}
