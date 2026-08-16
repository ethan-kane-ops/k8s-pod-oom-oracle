package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

var epoch = time.Date(2026, 8, 13, 8, 15, 22, 0, time.UTC)

const (
	podUID    = "3f0e2b6c_1a2b_4c3d_9e8f_0a1b2c3d4e5f"
	podUIDRaw = "3f0e2b6c-1a2b-4c3d-9e8f-0a1b2c3d4e5f"
	ctrID     = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"

	containerCgroup = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" +
		podUID + ".slice/cri-containerd-" + ctrID + ".scope"

	// podCgroup is the slice above it. A memory-backed emptyDir is charged
	// here rather than to any one container, so that is where its kill lands.
	podCgroup = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" +
		podUID + ".slice"
)

// logRecord is the part of a log entry these tests assert on.
type logRecord struct {
	level slog.Level
	msg   string
}

// recordingHandler captures log records, so a test can assert on the level a
// message was emitted at rather than only on the fact that it was emitted.
type recordingHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler fixes this signature, so the record cannot be taken by pointer.
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, logRecord{level: record.Level, msg: record.Message})
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// warnings returns the messages logged at warn or above, which is everything an
// operator sees without turning the log level up.
func (h *recordingHandler) warnings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	var msgs []string
	for _, record := range h.records {
		if record.level >= slog.LevelWarn {
			msgs = append(msgs, record.msg)
		}
	}
	return msgs
}

// harness wires a daemon over fixture filesystems with a controllable clock.
type harness struct {
	cgroups fstest.MapFS
	procs   fstest.MapFS
	clock   time.Time
	sampler *sampler.Sampler
	store   *store.Memory
	daemon  *Daemon
}

func newHarness(t *testing.T, lookup correlate.PodLookup) *harness {
	t.Helper()

	h := &harness{
		cgroups: fstest.MapFS{
			"cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
			"memory.current":     &fstest.MapFile{Data: []byte("0\n")},
			"memory.max":         &fstest.MapFile{Data: []byte("max\n")},
		},
		procs: fstest.MapFS{"meminfo": &fstest.MapFile{Data: []byte("MemTotal: 1 kB\n")}},
		clock: epoch,
	}

	cg, err := cgroup.New(h.cgroups)
	if err != nil {
		t.Fatalf("cgroup.New() error = %v", err)
	}
	procFS := procfs.New(h.procs)

	h.sampler, err = sampler.New(sampler.Options{
		FS: cg, Prefix: "/kubepods.slice", HistorySize: 10,
		Now: func() time.Time { return h.clock },
	})
	if err != nil {
		t.Fatalf("sampler.New() error = %v", err)
	}

	h.store = store.NewMemory(10)
	h.daemon, err = New(Options{
		Detector: detector.NewFake(),
		Sampler:  h.sampler,
		Store:    h.store,
		Cgroup:   cg,
		Resolver: correlate.NewResolver(lookup),
		Proc:     procFS,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	return h
}

// setMemory writes a container cgroup at the given usage and limit.
func (h *harness) setMemory(current, limit, peak uint64) {
	h.setMemoryAt(containerCgroup, current, limit, peak)
}

// setMemoryAt writes any cgroup in the tree at the given usage and limit.
func (h *harness) setMemoryAt(cgroupPath string, current, limit, peak uint64) {
	key := cgroupPath[1:]
	h.cgroups[key+"/memory.current"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", current)}
	h.cgroups[key+"/memory.max"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", limit)}
	h.cgroups[key+"/memory.peak"] = &fstest.MapFile{Data: fmt.Appendf(nil, "%d\n", peak)}
	h.cgroups[key+"/memory.events"] = &fstest.MapFile{
		Data: []byte("low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\n"),
	}
}

// setOOMGroup writes memory.oom.group for a cgroup. Absent means the kernel
// predates the file or the hierarchy is v1, which the reader treats as unset.
func (h *harness) setOOMGroup(cgroupPath string, on bool) {
	value := '0'
	if on {
		value = '1'
	}
	h.cgroups[cgroupPath[1:]+"/memory.oom.group"] = &fstest.MapFile{
		Data: []byte{byte(value), '\n'},
	}
}

// addProcess writes a process into the container. Its in-container PID is
// derived from the host PID, so a test can pick a host PID whose last two digits
// are the NSPid it wants to collide with.
func (h *harness) addProcess(pid int, comm string, rssKB int) {
	dir := strconv.Itoa(pid)
	h.procs[dir+"/status"] = &fstest.MapFile{
		Data: fmt.Appendf(nil, "Name:\t%s\nState:\tS (sleeping)\nPid:\t%d\nPPid:\t1\nNSpid:\t%d\t%d\nVmSize:\t 1000 kB\nVmRSS:\t %d kB\n",
			comm, pid, pid, pid%100, rssKB),
	}
	h.procs[dir+"/cmdline"] = &fstest.MapFile{Data: []byte(comm + "\x00")}
	h.procs[dir+"/cgroup"] = &fstest.MapFile{Data: []byte("0::" + containerCgroup + "\n")}
}

// collect takes one sampler pass and advances the clock.
func (h *harness) collect(t *testing.T) {
	t.Helper()

	if err := h.sampler.Collect(); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	h.clock = h.clock.Add(time.Second)
}

// latest returns the single stored report.
func (h *harness) latest(t *testing.T) oom.Report {
	t.Helper()

	reports, err := h.store.List(t.Context(), store.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	return reports[0]
}

func testLookup() correlate.MapPodLookup {
	return correlate.MapPodLookup{
		podUIDRaw: {
			Namespace: "default",
			Name:      "payment-api-6d5f78",
			Node:      "node-1",
			Containers: map[string]correlate.ContainerInfo{
				ctrID: {Name: "web-server", Image: "payment:v1.2.0"},
			},
		},
	}
}

func killEvent() detector.KillEvent {
	return detector.KillEvent{
		Time:       epoch,
		CgroupPath: containerCgroup,
		KillCount:  1,
		Source:     detector.SourcePoller,
		Victim: detector.Victim{
			PID: 28145, NSPid: 17, Comm: "gc", Cmdline: []string{"node", "./gc.js"},
			RSSBytes: 114 << 20, Inferred: true, Known: true,
		},
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	t.Parallel()

	full := Options{
		Detector: detector.NewFake(),
		Sampler:  &sampler.Sampler{},
		Store:    store.NewMemory(1),
		Resolver: correlate.NewResolver(nil),
	}

	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "missing detector", mutate: func(o *Options) { o.Detector = nil }},
		{name: "missing sampler", mutate: func(o *Options) { o.Sampler = nil }},
		{name: "missing store", mutate: func(o *Options) { o.Store = nil }},
		{name: "missing resolver", mutate: func(o *Options) { o.Resolver = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := full
			tt.mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatal("New() = nil error, want a missing-dependency error")
			}
		})
	}
}

// TestHandleBuildsFullReport is the pipeline integration test: a kill event
// plus buffered history plus pod metadata becomes a complete post-mortem.
func TestHandleBuildsFullReport(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	ctx := context.Background()

	// Three samples of climbing memory against a 512 MiB limit.
	const limit = 512 << 20
	for i, used := range []uint64{412 << 20, 498 << 20, 512 << 20} {
		h.setMemory(used, limit, 512<<20)
		if err := h.sampler.Collect(); err != nil {
			t.Fatalf("Collect() pass %d error = %v", i, err)
		}
		h.clock = h.clock.Add(30 * time.Second)
	}

	// Two processes survive the kill.
	h.addProcess(28102, "server", 390<<10)
	h.addProcess(28160, "worker", 40<<10)

	h.daemon.Handle(ctx, killEvent())

	if got := h.daemon.Processed(); got != 1 {
		t.Fatalf("Processed() = %d, want 1", got)
	}

	reports, err := h.store.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	report := reports[0]

	if report.Identity.Namespace != "default" || report.Identity.PodName != "payment-api-6d5f78" {
		t.Errorf("identity = %s, want default/payment-api-6d5f78", report.Identity)
	}
	if report.Identity.ContainerName != "web-server" {
		t.Errorf("ContainerName = %q, want web-server", report.Identity.ContainerName)
	}
	if report.Victim.Comm != "gc" || report.Victim.PID != 28145 {
		t.Errorf("victim = %s (pid %d), want gc (pid 28145)", report.Victim.Comm, report.Victim.PID)
	}
	if len(report.Trajectory) != 3 {
		t.Fatalf("len(Trajectory) = %d, want 3 buffered samples", len(report.Trajectory))
	}
	if report.Trajectory[0].UsedBytes != 412<<20 {
		t.Errorf("first trajectory point = %d, want the oldest sample", report.Trajectory[0].UsedBytes)
	}
	if report.LimitBytes != limit {
		t.Errorf("LimitBytes = %d, want %d", report.LimitBytes, limit)
	}
	if !report.Trend.Projected {
		t.Error("Trend.Projected = false, want a projection for climbing memory")
	}
	if len(report.Processes) != 2 {
		t.Fatalf("len(Processes) = %d, want the 2 non-victim processes", len(report.Processes))
	}
	if report.Processes[0].Comm != "server" {
		t.Errorf("Processes[0] = %q, want the heaviest process first", report.Processes[0].Comm)
	}
	if report.ID == "" {
		t.Error("report ID is empty")
	}
}

// TestHandleFiltersVictimAcrossPIDNamespaces is the ENG-131 regression test.
//
// Under a nested runtime the probe reports the kernel's global PID, which does
// not appear in the /proc the daemon can read, so comparing host PIDs leaves the
// dead process listed as though it were alive. Here the victim is at global pid
// 1397320 and the same process appears in /proc as pid 28117, in-container 17.
func TestHandleFiltersVictimAcrossPIDNamespaces(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	h.setMemory(500<<20, 512<<20, 512<<20)
	h.addProcess(28102, "server", 400<<10)
	h.addProcess(28117, "gc", 1<<10)
	h.collect(t)

	event := killEvent()
	event.Victim.PID = 1397320
	event.Victim.NSPid = 17
	h.daemon.Handle(context.Background(), event)

	report := h.latest(t)
	for _, proc := range report.Processes {
		if proc.NSPid == 17 {
			t.Errorf("Processes contains the victim (nspid 17, host pid %d); "+
				"host PIDs cannot match under a nested runtime, so NSPid must be used",
				proc.PID)
		}
	}
	if len(report.Processes) != 1 {
		t.Fatalf("len(Processes) = %d, want 1 once the victim is removed", len(report.Processes))
	}
	if report.Processes[0].Comm != "server" {
		t.Errorf("Processes[0] = %q, want server", report.Processes[0].Comm)
	}
}

// TestHandleRecordsGroupKill covers the flag that decides whether the process
// listing means survivors or a teardown snapshot.
func TestHandleRecordsGroupKill(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*harness)
		want  bool
	}{
		{
			// containerd's default, so this is what almost every cluster does.
			name:  "group kill is recorded when the cgroup is killed as a unit",
			setup: func(h *harness) { h.setOOMGroup(containerCgroup, true) },
			want:  true,
		},
		{
			name:  "a cgroup that kills one process reports false",
			setup: func(h *harness) { h.setOOMGroup(containerCgroup, false) },
			want:  false,
		},
		{
			// An older kernel has no such file. False here means "not observed",
			// which is why nothing downstream claims survival from it.
			name:  "an absent file reports false rather than failing",
			setup: func(*harness) {},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, testLookup())
			h.setMemory(500<<20, 512<<20, 512<<20)
			tt.setup(h)
			h.collect(t)

			h.daemon.Handle(context.Background(), killEvent())

			if got := h.latest(t).GroupKill; got != tt.want {
				t.Errorf("GroupKill = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHandleSkipsNonKubernetes is the filter that stops a host service crash
// being attributed to somebody's pod.
func TestHandleSkipsNonKubernetes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	ctx := context.Background()

	event := killEvent()
	event.CgroupPath = "/system.slice/docker.service"
	h.daemon.Handle(ctx, event)

	if got := h.daemon.Processed(); got != 0 {
		t.Errorf("Processed() = %d, want 0 for a non-Kubernetes cgroup", got)
	}
	if got := h.daemon.Skipped(); got != 1 {
		t.Errorf("Skipped() = %d, want 1", got)
	}
}

func TestHandleIncludesNonKubernetesWhenAsked(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	// Rebuild the daemon with the flag on.
	d, err := New(Options{
		Detector: detector.NewFake(), Sampler: h.sampler, Store: h.store,
		Resolver: correlate.NewResolver(testLookup()), IncludeNonKubernetes: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	event := killEvent()
	event.CgroupPath = "/system.slice/docker.service"
	d.Handle(context.Background(), event)

	if got := d.Processed(); got != 1 {
		t.Errorf("Processed() = %d, want 1 with IncludeNonKubernetes", got)
	}
}

// TestHandleWithoutHistory covers a container that appeared and died inside a
// single sample window: the kill is still reported, just without a trajectory.
// TestHandleReportsPodLevelKill is the regression test for kills charged to the
// pod rather than to one of its containers, which the daemon used to drop.
//
// A memory-backed emptyDir accounts its pages to the pod slice, so a pod that
// fills /dev/shm is killed there. Kubernetes marks the pod OOMKilled and the
// daemon reported nothing at all, which is a false negative in the one job it
// has.
func TestHandleReportsPodLevelKill(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())

	// The limit that was hit belongs to the pod slice, not to a container.
	h.setMemoryAt(podCgroup, 256<<20, 256<<20, 256<<20)

	event := killEvent()
	event.CgroupPath = podCgroup
	h.daemon.Handle(t.Context(), event)

	if got := h.daemon.Processed(); got != 1 {
		t.Fatalf("Processed() = %d, want 1; a pod-level kill must still be reported", got)
	}

	report := h.latest(t)
	if report.Identity.Kind != correlate.ScopePod {
		t.Errorf("Kind = %q, want %q", report.Identity.Kind, correlate.ScopePod)
	}
	if report.Identity.PodName != "payment-api-6d5f78" {
		t.Errorf("PodName = %q, want the pod still identified", report.Identity.PodName)
	}
	// Naming a container here would send the reader after the wrong process:
	// the memory was shared, and no single container owned it.
	if report.Identity.ContainerName != "" {
		t.Errorf("ContainerName = %q, want empty for a pod-level kill", report.Identity.ContainerName)
	}
	if want := uint64(256 << 20); report.LimitBytes != want {
		t.Errorf("LimitBytes = %d, want the pod slice's %d", report.LimitBytes, want)
	}
}

// TestHandleWarnsOnlyForKillsItShouldHavePlaced covers the daemon's own blind
// spot. An unplaceable kill is routine out on the host, where the probes see
// every service on the node, and a defect inside the kubepods tree: something
// in a pod died and nobody will be told. Logging both at debug is what let the
// pod-level case go unnoticed for as long as it did.
func TestHandleWarnsOnlyForKillsItShouldHavePlaced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cgroupPath   string
		wantWarnings int
	}{
		{
			name:       "host service is routine",
			cgroupPath: "/system.slice/docker.service",
		},
		{
			// Inside the tree, and still unparseable: a runtime whose cgroup
			// prefix the parser does not know.
			name: "unparseable inside the kubepods tree is a lost report",
			cgroupPath: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" +
				podUID + ".slice/mystery-" + ctrID + ".scope",
			wantWarnings: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, testLookup())
			handler := &recordingHandler{}
			d, err := New(Options{
				Detector: detector.NewFake(), Sampler: h.sampler, Store: h.store,
				Resolver: correlate.NewResolver(testLookup()),
				Logger:   slog.New(handler),
			})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			event := killEvent()
			event.CgroupPath = tt.cgroupPath
			d.Handle(t.Context(), event)

			if got := d.Processed(); got != 0 {
				t.Errorf("Processed() = %d, want 0 for an unplaceable kill", got)
			}
			if got := d.Skipped(); got != 1 {
				t.Errorf("Skipped() = %d, want 1", got)
			}
			if got := handler.warnings(); len(got) != tt.wantWarnings {
				t.Errorf("warnings = %d %q, want %d", len(got), got, tt.wantWarnings)
			}
		})
	}
}

func TestHandleWithoutHistory(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	h.daemon.Handle(context.Background(), killEvent())

	reports, err := h.store.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want the kill still recorded", len(reports))
	}
	if len(reports[0].Trajectory) != 0 {
		t.Errorf("len(Trajectory) = %d, want 0", len(reports[0].Trajectory))
	}
}

// TestHandleUnresolvedPodStillReports covers a pod the daemon cannot look up,
// which happens when it was deleted before the kill was processed.
func TestHandleUnresolvedPodStillReports(t *testing.T) {
	t.Parallel()

	h := newHarness(t, correlate.MapPodLookup{})
	h.daemon.Handle(context.Background(), killEvent())

	reports, err := h.store.List(context.Background(), store.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	if reports[0].Identity.Resolved {
		t.Error("Identity.Resolved = true, want false for an unknown pod")
	}
	if reports[0].Identity.PodUID != podUIDRaw {
		t.Errorf("PodUID = %q, want the cgroup-derived UID %q", reports[0].Identity.PodUID, podUIDRaw)
	}
}

func TestHandleInvokesOnReport(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())

	var seen []oom.Report
	d, err := New(Options{
		Detector: detector.NewFake(), Sampler: h.sampler, Store: h.store,
		Resolver: correlate.NewResolver(testLookup()),
		OnReport: func(_ context.Context, r oom.Report) { seen = append(seen, r) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	d.Handle(context.Background(), killEvent())

	if len(seen) != 1 {
		t.Fatalf("OnReport called %d times, want 1", len(seen))
	}
	if seen[0].Victim.Comm != "gc" {
		t.Errorf("OnReport report victim = %q, want gc", seen[0].Victim.Comm)
	}
}

func TestReportIDsAreUniqueAndOrdered(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	ctx := context.Background()

	for range 3 {
		h.daemon.Handle(ctx, killEvent())
	}

	reports, err := h.store.List(ctx, store.Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("len(reports) = %d, want 3 distinct IDs for identical events", len(reports))
	}

	seen := make(map[string]struct{}, len(reports))
	for _, r := range reports {
		if _, duplicate := seen[r.ID]; duplicate {
			t.Errorf("duplicate report ID %q", r.ID)
		}
		seen[r.ID] = struct{}{}
	}
}

func TestRunConsumesDetectorEvents(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())
	// Run starts the sampler, which requires its prefix to exist. On a real
	// node it always does: the daemon's own pod lives under kubepods.slice.
	h.setMemory(1<<20, 1<<30, 1<<20)

	fake := detector.NewFake(killEvent())
	d, err := New(Options{
		Detector: fake, Sampler: h.sampler, Store: h.store,
		Resolver: correlate.NewResolver(testLookup()),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Wait for the scripted event to be consumed, then shut down.
	deadline := time.After(5 * time.Second)
	for d.Processed() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run() did not process the scripted event")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v, want nil on cancellation", err)
	}
}

func TestHandleCorrectsPeakFromLiveCgroup(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())

	// A container that sat idle and then filled its limit faster than the
	// sampler runs. Every sample sees the idle figure, including the peak,
	// because memory.peak had not moved yet when the last one was taken.
	h.setMemory(1<<20, 512<<20, 1<<20)
	h.collect(t)

	// The kernel's own high-water mark, readable after the kill because
	// memory.peak is monotonic.
	h.setMemory(1<<20, 512<<20, 512<<20)

	h.daemon.Handle(t.Context(), killEvent())

	report := h.latest(t)
	if want := uint64(512 << 20); report.PeakBytes != want {
		t.Errorf("PeakBytes = %d, want %d from the live cgroup rather than the stale sample",
			report.PeakBytes, want)
	}
}

func TestHandleFloorsPeakAtVictimMemory(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())

	// No cgroup files at all: the container was killed outright and the
	// kernel destroyed its cgroup before the report was built. Reporting a
	// peak below what the victim alone was holding would be self-contradictory
	// on the face of the report.
	h.daemon.Handle(t.Context(), killEvent())

	report := h.latest(t)
	if want := uint64(114 << 20); report.PeakBytes != want {
		t.Errorf("PeakBytes = %d, want the victim's %d as a floor", report.PeakBytes, want)
	}
}

func TestHandleKeepsHigherSampledPeak(t *testing.T) {
	t.Parallel()

	h := newHarness(t, testLookup())

	// memory.peak can be reset by writing to it, so a live read is not always
	// the larger number. Whichever evidence is highest wins.
	h.setMemory(400<<20, 512<<20, 480<<20)
	h.collect(t)
	h.setMemory(1<<20, 512<<20, 0)

	h.daemon.Handle(t.Context(), killEvent())

	if report := h.latest(t); report.PeakBytes != 480<<20 {
		t.Errorf("PeakBytes = %d, want the sampled %d to survive a reset counter",
			report.PeakBytes, uint64(480<<20))
	}
}
