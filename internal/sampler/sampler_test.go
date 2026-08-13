package sampler

import (
	"context"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

const (
	containerA = "/kubepods.slice/pod-a/container"
	containerB = "/kubepods.slice/pod-b/container"
)

// tree builds a mutable unified-hierarchy fixture. Tests add and remove
// container directories between passes to simulate node churn.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"memory.current":     &fstest.MapFile{Data: []byte("0\n")},
		"memory.max":         &fstest.MapFile{Data: []byte("max\n")},
	}
}

// addContainer writes the files a single container cgroup exposes.
func addContainer(t fstest.MapFS, path, current, limit string) {
	key := path[1:] // fstest.MapFS keys are relative
	t[key+"/memory.current"] = &fstest.MapFile{Data: []byte(current + "\n")}
	t[key+"/memory.max"] = &fstest.MapFile{Data: []byte(limit + "\n")}
	t[key+"/memory.events"] = &fstest.MapFile{Data: []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n")}
}

// removeContainer deletes every file belonging to a container cgroup.
func removeContainer(t fstest.MapFS, path string) {
	key := path[1:]
	for name := range t {
		if len(name) > len(key) && name[:len(key)] == key {
			delete(t, name)
		}
	}
}

// clock is a deterministic time source advancing on demand.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newSampler wires a Sampler over a fixture tree with a controllable clock.
func newSampler(t *testing.T, tree fstest.MapFS, opts Options) (*Sampler, *clock) {
	t.Helper()

	cg, err := cgroup.New(tree)
	if err != nil {
		t.Fatalf("cgroup.New() error = %v", err)
	}

	c := &clock{now: epoch}
	opts.FS = cg
	opts.Now = c.Now

	s, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s, c
}

func TestNewRequiresCgroupFS(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{}); err == nil {
		t.Fatal("New() without a cgroup filesystem = nil error, want error")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	s, _ := newSampler(t, tree(), Options{})

	if s.interval != DefaultInterval {
		t.Errorf("interval = %v, want %v", s.interval, DefaultInterval)
	}
	if s.historySize != DefaultHistorySize {
		t.Errorf("historySize = %d, want %d", s.historySize, DefaultHistorySize)
	}
	if s.log == nil {
		t.Error("logger is nil; Collect would panic on the first read failure")
	}
}

func TestCollectBuildsHistory(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "104857600", "536870912")
	s, c := newSampler(t, fixture, Options{Prefix: "/kubepods.slice", HistorySize: 10})

	// Three passes, one second apart, with memory climbing each time.
	for i, current := range []string{"104857600", "209715200", "314572800"} {
		if i > 0 {
			c.Advance(time.Second)
			addContainer(fixture, containerA, current, "536870912")
		}
		if err := s.Collect(); err != nil {
			t.Fatalf("Collect() pass %d error = %v", i, err)
		}
	}

	history, ok := s.History(containerA)
	if !ok {
		t.Fatalf("History(%q) reported not tracked; tracked = %v", containerA, s.Tracked())
	}
	if got := currents(history); !reflect.DeepEqual(got, []uint64{104857600, 209715200, 314572800}) {
		t.Errorf("history = %v, want the three readings oldest first", got)
	}

	// Timestamps must come from the injected clock, not the wall clock.
	if !history[0].Time.Equal(epoch) {
		t.Errorf("first sample time = %v, want %v", history[0].Time, epoch)
	}
	if want := epoch.Add(2 * time.Second); !history[2].Time.Equal(want) {
		t.Errorf("last sample time = %v, want %v", history[2].Time, want)
	}
}

func TestCollectRespectsHistorySize(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	s, c := newSampler(t, fixture, Options{Prefix: "/kubepods.slice", HistorySize: 3})

	for range 10 {
		if err := s.Collect(); err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		c.Advance(time.Second)
	}

	history, ok := s.History(containerA)
	if !ok {
		t.Fatal("History() reported not tracked")
	}
	if len(history) != 3 {
		t.Errorf("len(history) = %d, want 3", len(history))
	}
}

func TestCollectEvictsVanishedCgroups(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	addContainer(fixture, containerB, "2048", "max")
	s, c := newSampler(t, fixture, Options{Prefix: "/kubepods.slice"})

	if err := s.Collect(); err != nil {
		t.Fatalf("first Collect() error = %v", err)
	}
	if got := s.Tracked(); len(got) != 2 {
		t.Fatalf("Tracked() = %v, want both containers", got)
	}

	// Container B is torn down between passes, as happens constantly on a node.
	removeContainer(fixture, containerB)
	if err := s.Collect(); err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}

	// History survives the retention window so a post-mortem can still read it.
	if _, ok := s.History(containerB); !ok {
		t.Fatal("History() dropped the container immediately; its trajectory is the report")
	}

	// Past the retention window it is finally dropped.
	c.Advance(2 * DefaultRetention)
	if err := s.Collect(); err != nil {
		t.Fatalf("third Collect() error = %v", err)
	}

	if got, want := s.Tracked(), []string{containerA}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tracked() = %v, want %v; stale rings leak memory on a churning node", got, want)
	}
	if _, ok := s.History(containerB); ok {
		t.Error("History() still returns the evicted container")
	}
}

func TestCollectSurvivesUnreadableCgroup(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	// A cgroup advertising a memory controller whose limit file is corrupt.
	fixture["kubepods.slice/pod-broken/container/memory.current"] = &fstest.MapFile{Data: []byte("2048\n")}
	fixture["kubepods.slice/pod-broken/container/memory.max"] = &fstest.MapFile{Data: []byte("garbage\n")}

	s, _ := newSampler(t, fixture, Options{Prefix: "/kubepods.slice"})

	if err := s.Collect(); err != nil {
		t.Fatalf("Collect() error = %v; one bad cgroup must not fail the pass", err)
	}
	if _, ok := s.History(containerA); !ok {
		t.Error("the healthy container was not sampled alongside the broken one")
	}
	if _, ok := s.History("/kubepods.slice/pod-broken/container"); ok {
		t.Error("the broken cgroup was recorded despite failing to parse")
	}
}

func TestCollectToleratesMissingPSI(t *testing.T) {
	t.Parallel()

	// The fixture has no memory.pressure anywhere, as on a kernel without
	// CONFIG_PSI. Collection must still succeed with a zero-valued PSI.
	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	s, _ := newSampler(t, fixture, Options{Prefix: "/kubepods.slice"})

	if err := s.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	latest, ok := s.Latest(containerA)
	if !ok {
		t.Fatal("Latest() reported not tracked")
	}
	if latest.PSI != (cgroup.PSI{}) {
		t.Errorf("PSI = %+v, want the zero value when unsupported", latest.PSI)
	}
}

func TestCollectMissingPrefixIsAnError(t *testing.T) {
	t.Parallel()

	s, _ := newSampler(t, tree(), Options{Prefix: "/does-not-exist"})

	if err := s.Collect(); err == nil {
		t.Fatal("Collect() with a bad prefix = nil error; a typo would look like an idle node")
	}
}

func TestTrend(t *testing.T) {
	t.Parallel()

	fixture := tree()
	s, c := newSampler(t, fixture, Options{Prefix: "/kubepods.slice", HistorySize: 10})

	const mib = 1 << 20
	for i, current := range []string{"10485760", "20971520", "31457280"} {
		addContainer(fixture, containerA, current, "104857600")
		if err := s.Collect(); err != nil {
			t.Fatalf("Collect() pass %d error = %v", i, err)
		}
		c.Advance(time.Second)
	}

	trend, ok := s.Trend(containerA)
	if !ok {
		t.Fatal("Trend() reported not tracked")
	}
	if !trend.Projected {
		t.Fatal("Projected = false, want a projection for steadily climbing memory")
	}
	if got, want := trend.BytesPerSecond, float64(10*mib); got != want {
		t.Errorf("BytesPerSecond = %v, want %v", got, want)
	}
	// 100 MiB limit, 30 MiB used, 70 MiB of headroom at 10 MiB/s.
	if got, want := trend.TimeToLimit, 7*time.Second; got != want {
		t.Errorf("TimeToLimit = %v, want %v", got, want)
	}
}

func TestTrendUntrackedCgroup(t *testing.T) {
	t.Parallel()

	s, _ := newSampler(t, tree(), Options{Prefix: "/"})

	if _, ok := s.Trend("/nope"); ok {
		t.Error("Trend() on an untracked cgroup reported ok")
	}
	if _, ok := s.Latest("/nope"); ok {
		t.Error("Latest() on an untracked cgroup reported ok")
	}
}

func TestRunCollectsThenStopsOnCancel(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	s, _ := newSampler(t, fixture, Options{Prefix: "/kubepods.slice", Interval: time.Hour})

	// Cancelled up front: Run must still perform its initial pass, then return
	// nil rather than waiting out the interval.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v, want nil on cancellation", err)
	}
	if _, ok := s.History(containerA); !ok {
		t.Error("Run() returned without performing its initial collection")
	}
}

func TestRunPropagatesCollectError(t *testing.T) {
	t.Parallel()

	s, _ := newSampler(t, tree(), Options{Prefix: "/does-not-exist", Interval: time.Hour})

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run() = nil error, want the initial Collect failure surfaced")
	}
}

func TestTrackedIsSorted(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerB, "2048", "max")
	addContainer(fixture, containerA, "1024", "max")
	s, _ := newSampler(t, fixture, Options{Prefix: "/kubepods.slice"})

	if err := s.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	if got, want := s.Tracked(), []string{containerA, containerB}; !reflect.DeepEqual(got, want) {
		t.Errorf("Tracked() = %v, want %v", got, want)
	}
}

func TestConcurrentReadsDuringCollect(t *testing.T) {
	t.Parallel()

	fixture := tree()
	addContainer(fixture, containerA, "1024", "max")
	s, _ := newSampler(t, fixture, Options{Prefix: "/kubepods.slice"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			s.Tracked()
			s.History(containerA)
			s.Trend(containerA)
			s.Latest(containerA)
		}
	}()

	for range 100 {
		if err := s.Collect(); err != nil {
			t.Errorf("Collect() error = %v", err)
			break
		}
	}
	<-done
}
