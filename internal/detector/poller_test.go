package detector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

var epoch = time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

const (
	containerA = "/kubepods.slice/pod-a/container"
	containerB = "/kubepods.slice/pod-b/container"
)

// fixture holds the mutable cgroup and proc trees a test drives.
type fixture struct {
	cgroups fstest.MapFS
	procs   fstest.MapFS
	// members records which PIDs are in which cgroup, so cgroup.procs can be
	// rewritten in full whenever one is added or removed.
	members map[string][]int
}

func newFixture() *fixture {
	return &fixture{
		cgroups: fstest.MapFS{
			"cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
			"memory.current":     &fstest.MapFile{Data: []byte("0\n")},
			"memory.max":         &fstest.MapFile{Data: []byte("max\n")},
		},
		procs: fstest.MapFS{
			"meminfo": &fstest.MapFile{Data: []byte("MemTotal: 16777216 kB\n")},
		},
		members: map[string][]int{},
	}
}

// setCgroup writes a container cgroup with the given cumulative kill count.
func (f *fixture) setCgroup(path string, oomKills int) {
	if _, ok := f.members[path]; !ok {
		// The kernel creates cgroup.procs with the cgroup, empty. A fixture
		// without it makes an empty cgroup indistinguishable from a deleted one.
		f.members[path] = nil
		f.writeProcs(path)
	}
	key := path[1:]
	f.cgroups[key+"/memory.current"] = &fstest.MapFile{Data: []byte("1048576\n")}
	f.cgroups[key+"/memory.max"] = &fstest.MapFile{Data: []byte("67108864\n")}
	f.cgroups[key+"/memory.events"] = &fstest.MapFile{
		Data: fmt.Appendf(nil, "low 0\nhigh 0\nmax 0\noom %d\noom_kill %d\n", oomKills, oomKills),
	}
}

func (f *fixture) removeCgroup(path string) {
	key := path[1:]
	for name := range f.cgroups {
		if strings.HasPrefix(name, key) {
			delete(f.cgroups, name)
		}
	}
}

// setProcess writes a process into the proc tree, attached to a cgroup.
//
// The cgroup's own cgroup.procs is kept in step, because that is where the
// poller reads membership from: /proc/<pid>/cgroup is namespace-relative and
// unusable from an unprivileged pod.
func (f *fixture) setProcess(pid int, comm, cgroupPath string, rssKB int) {
	f.members[cgroupPath] = append(f.members[cgroupPath], pid)
	f.writeProcs(cgroupPath)

	dir := strconv.Itoa(pid)
	f.procs[dir+"/status"] = &fstest.MapFile{
		Data: fmt.Appendf(nil, "Name:\t%s\nState:\tS (sleeping)\nPid:\t%d\nPPid:\t1\nNSpid:\t%d\t1\nVmSize:\t 100000 kB\nVmRSS:\t %d kB\n",
			comm, pid, pid, rssKB),
	}
	f.procs[dir+"/cmdline"] = &fstest.MapFile{Data: []byte(comm + "\x00")}
	f.procs[dir+"/cgroup"] = &fstest.MapFile{Data: []byte("0::" + cgroupPath + "\n")}
}

func (f *fixture) removeProcess(pid int) {
	for cgroupPath, pids := range f.members {
		kept := pids[:0]
		for _, member := range pids {
			if member != pid {
				kept = append(kept, member)
			}
		}
		f.members[cgroupPath] = kept
		f.writeProcs(cgroupPath)
	}

	dir := strconv.Itoa(pid)
	for name := range f.procs {
		if strings.HasPrefix(name, dir+"/") {
			delete(f.procs, name)
		}
	}
}

// writeProcs rewrites a cgroup's membership file from the fixture's record.
func (f *fixture) writeProcs(cgroupPath string) {
	var list []byte
	for _, pid := range f.members[cgroupPath] {
		list = fmt.Appendf(list, "%d\n", pid)
	}
	f.cgroups[cgroupPath[1:]+"/cgroup.procs"] = &fstest.MapFile{Data: list}
}

// newPoller wires a Poller over the fixture with a deterministic clock.
func (f *fixture) newPoller(t *testing.T, withProc bool) *Poller {
	t.Helper()

	cg, err := cgroup.New(f.cgroups)
	if err != nil {
		t.Fatalf("cgroup.New() error = %v", err)
	}

	opts := PollerOptions{Cgroup: cg, Prefix: "/kubepods.slice", Now: func() time.Time { return epoch }}
	if withProc {
		opts.Proc = procfs.New(f.procs)
	}

	p, err := NewPoller(opts)
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// drain collects everything currently buffered on the event channel.
func drain(t *testing.T, events <-chan KillEvent) []KillEvent {
	t.Helper()

	var got []KillEvent
	for {
		select {
		case event := <-events:
			got = append(got, event)
		default:
			return got
		}
	}
}

func TestNewPollerRequiresCgroupFS(t *testing.T) {
	t.Parallel()

	if _, err := NewPoller(PollerOptions{}); err == nil {
		t.Fatal("NewPoller() without a cgroup filesystem = nil error, want error")
	}
}

func TestPollerSourceIsPoller(t *testing.T) {
	t.Parallel()

	f := newFixture()
	if got := f.newPoller(t, false).Source(); got != SourcePoller {
		t.Errorf("Source() = %q, want %q", got, SourcePoller)
	}
}

// TestPollerDoesNotReplayHistoryOnStartup is the behaviour that keeps a daemon
// restart from flooding a cluster with alerts. Containers that were already
// killed carry a non-zero counter, and that is not news.
func TestPollerDoesNotReplayHistoryOnStartup(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 3)
	p := f.newPoller(t, false)

	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}

	if got := drain(t, p.Events()); len(got) != 0 {
		t.Errorf("Prime() emitted %d events for a pre-existing kill count, want 0", len(got))
	}
}

func TestPollerEmitsOnCounterIncrease(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	p := f.newPoller(t, false)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	// No kill yet.
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if got := drain(t, events); len(got) != 0 {
		t.Fatalf("Poll() emitted %d events with an unchanged counter, want 0", len(got))
	}

	// The kernel kills something.
	f.setCgroup(containerA, 1)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 1 {
		t.Fatalf("Poll() emitted %d events after a kill, want 1", len(got))
	}
	if got[0].CgroupPath != containerA {
		t.Errorf("CgroupPath = %q, want %q", got[0].CgroupPath, containerA)
	}
	if got[0].KillCount != 1 {
		t.Errorf("KillCount = %d, want 1", got[0].KillCount)
	}
	if got[0].Source != SourcePoller {
		t.Errorf("Source = %q, want %q", got[0].Source, SourcePoller)
	}
	if !got[0].Time.Equal(epoch) {
		t.Errorf("Time = %v, want the injected clock value %v", got[0].Time, epoch)
	}

	// A further pass with no new kill must stay quiet.
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if got := drain(t, events); len(got) != 0 {
		t.Errorf("Poll() re-emitted %d events for the same kill, want 0", len(got))
	}
}

// TestPollerReadsGroupKillAtDetection covers the flag that tells a report
// whether its process listing is a survivor list or a teardown snapshot.
//
// The poller reads it when it notices the kill rather than leaving it to report
// assembly, which is later still. It cannot close the window the way a traced
// kill does, so the unknown case is the one that has to stay honest: a cgroup
// that is already gone must produce nil, never false.
func TestPollerReadsGroupKillAtDetection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		oomGroup string
		want     *bool
	}{
		{
			// containerd's default, and the case the flag exists to describe.
			name:     "a group-killed cgroup reports true",
			oomGroup: "1\n",
			want:     ptr(true),
		},
		{
			name:     "a cgroup that kills one process reports false",
			oomGroup: "0\n",
			want:     ptr(false),
		},
		{
			// The file arrived in 4.19. The cgroup is still there, so its
			// absence is an answer rather than a failure to read.
			name:     "an absent file inside a live cgroup reports false",
			oomGroup: "",
			want:     ptr(false),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture()
			f.setCgroup(containerA, 0)
			if tt.oomGroup != "" {
				f.cgroups[containerA[1:]+"/memory.oom.group"] = &fstest.MapFile{
					Data: []byte(tt.oomGroup),
				}
			}

			p := f.newPoller(t, false)
			if err := p.Prime(); err != nil {
				t.Fatalf("Prime() error = %v", err)
			}
			if err := p.Poll(context.Background()); err != nil {
				t.Fatalf("Poll() error = %v", err)
			}

			f.setCgroup(containerA, 1)
			if err := p.Poll(context.Background()); err != nil {
				t.Fatalf("Poll() error = %v", err)
			}

			got := drain(t, p.Events())
			if len(got) != 1 {
				t.Fatalf("Poll() emitted %d events, want 1", len(got))
			}
			assertGroupKill(t, got[0].GroupKill, tt.want)
		})
	}
}

// TestPollerReadGroupKillReportsUnknownForAVanishedCgroup exercises the branch
// the tri-state exists for.
//
// It calls readGroupKill directly rather than driving a poll, because the
// situation cannot be staged through the polling loop: detectLocked reads
// memory.events first, so a cgroup that has fully vanished produces no event at
// all. The real case is a race between those two reads, and what matters is that
// the losing side yields nil. Reporting false there would state that a container
// survived a kill that took the whole cgroup down.
func TestPollerReadGroupKillReportsUnknownForAVanishedCgroup(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	p := f.newPoller(t, false)

	if got := p.readGroupKill(containerA + "-torn-down"); got != nil {
		t.Errorf("readGroupKill() = %v, want unknown: nothing read the file", *got)
	}
	if got := p.readGroupKill(containerA); got == nil || *got {
		t.Errorf("readGroupKill() on a live cgroup = %v, want false", got)
	}
}

// assertGroupKill compares two tri-states and names the state in the failure.
func assertGroupKill(t *testing.T, got, want *bool) {
	t.Helper()

	switch {
	case want == nil && got != nil:
		t.Errorf("GroupKill = %v, want unknown", *got)
	case want != nil && got == nil:
		t.Errorf("GroupKill = unknown, want %v", *want)
	case want != nil && *got != *want:
		t.Errorf("GroupKill = %v, want %v", *got, *want)
	}
}

// ptr is the shortest way to build the pointer a tri-state field needs.
func ptr[T any](v T) *T { return &v }

func TestPollerEmitsPerCgroup(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	f.setCgroup(containerB, 0)
	p := f.newPoller(t, false)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	f.setCgroup(containerA, 1)
	f.setCgroup(containerB, 2)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 2 {
		t.Fatalf("Poll() emitted %d events, want one per affected cgroup", len(got))
	}

	byPath := make(map[string]uint64, len(got))
	for _, event := range got {
		byPath[event.CgroupPath] = event.KillCount
	}
	if byPath[containerA] != 1 || byPath[containerB] != 2 {
		t.Errorf("kill counts = %v, want {%s:1, %s:2}", byPath, containerA, containerB)
	}
}

// TestPollerInfersVictim covers the poller's central trick: the victim is
// already gone from /proc by the time the counter is read, so it is identified
// by diffing against the previous snapshot.
func TestPollerInfersVictim(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	f.setProcess(28102, "server", containerA, 390_000)
	f.setProcess(28145, "gc", containerA, 114_000)
	p := f.newPoller(t, true)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	// The garbage collector is killed and the counter rises.
	f.removeProcess(28145)
	f.setCgroup(containerA, 1)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 1 {
		t.Fatalf("Poll() emitted %d events, want 1", len(got))
	}

	victim := got[0].Victim
	if !victim.Known {
		t.Fatal("Victim.Known = false, want the disappeared process identified")
	}
	if !victim.Inferred {
		t.Error("Victim.Inferred = false; the poller deduces the victim and must say so")
	}
	if victim.PID != 28145 {
		t.Errorf("Victim.PID = %d, want 28145", victim.PID)
	}
	if victim.Comm != "gc" {
		t.Errorf("Victim.Comm = %q, want %q", victim.Comm, "gc")
	}
	if victim.RSSBytes != 114_000*1024 {
		t.Errorf("Victim.RSSBytes = %d, want the last-known RSS %d", victim.RSSBytes, 114_000*1024)
	}
	if victim.NSPid != 1 {
		t.Errorf("Victim.NSPid = %d, want the container-local pid 1", victim.NSPid)
	}
}

// TestPollerPicksHeaviestOfSeveralVanished pins the tie-break. When more than
// one process disappears in an interval, the kernel's badness heuristic targets
// the largest, so that is the best available guess.
func TestPollerPicksHeaviestOfSeveralVanished(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	f.setProcess(100, "small", containerA, 10_000)
	f.setProcess(200, "large", containerA, 500_000)
	f.setProcess(300, "medium", containerA, 50_000)
	p := f.newPoller(t, true)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	// All three exit in the same interval.
	f.removeProcess(100)
	f.removeProcess(200)
	f.removeProcess(300)
	f.setCgroup(containerA, 1)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 1 {
		t.Fatalf("Poll() emitted %d events, want 1", len(got))
	}
	if got[0].Victim.PID != 200 {
		t.Errorf("Victim.PID = %d, want 200, the heaviest of the vanished processes", got[0].Victim.PID)
	}
}

func TestPollerWithoutProcFSReportsUnknownVictim(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	p := f.newPoller(t, false)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	f.setCgroup(containerA, 1)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 1 {
		t.Fatalf("Poll() emitted %d events, want the kill still reported", len(got))
	}
	if got[0].Victim.Known {
		t.Error("Victim.Known = true without a proc filesystem, want false")
	}
}

func TestPollerNoVictimWhenNothingDisappeared(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	f.setProcess(100, "server", containerA, 10_000)
	p := f.newPoller(t, true)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	// Counter rises but every process survived, as happens when the kill hit a
	// short-lived child the poller never sampled.
	f.setCgroup(containerA, 1)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != 1 {
		t.Fatalf("Poll() emitted %d events, want the kill still reported", len(got))
	}
	if got[0].Victim.Known {
		t.Error("Victim.Known = true, want false when no process disappeared")
	}
}

func TestPollerTreatsNewCgroupAsBaseline(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	p := f.newPoller(t, false)

	ctx := context.Background()
	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}
	events := p.Events()

	// A container appears mid-run already carrying kills, as a restarted pod
	// with a recycled cgroup can.
	f.setCgroup(containerB, 5)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	if got := drain(t, events); len(got) != 0 {
		t.Errorf("Poll() emitted %d events for a newly seen cgroup, want 0", len(got))
	}

	// A subsequent kill on it is real news.
	f.setCgroup(containerB, 6)
	if err := p.Poll(ctx); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if got := drain(t, events); len(got) != 1 {
		t.Errorf("Poll() emitted %d events for a genuine new kill, want 1", len(got))
	}
}

func TestPollerEvictsVanishedCgroups(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	f.setCgroup(containerB, 0)
	p := f.newPoller(t, false)

	if err := p.Prime(); err != nil {
		t.Fatalf("Prime() error = %v", err)
	}

	f.removeCgroup(containerB)
	if err := p.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	p.mu.Lock()
	_, stillTracked := p.counters[containerB]
	p.mu.Unlock()

	if stillTracked {
		t.Error("counters still hold the removed cgroup; state leaks on a churning node")
	}
}

func TestPollerStartFailsOnBadPrefix(t *testing.T) {
	t.Parallel()

	f := newFixture()
	cg, err := cgroup.New(f.cgroups)
	if err != nil {
		t.Fatalf("cgroup.New() error = %v", err)
	}
	p, err := NewPoller(PollerOptions{Cgroup: cg, Prefix: "/nope"})
	if err != nil {
		t.Fatalf("NewPoller() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if _, err := p.Start(context.Background()); err == nil {
		t.Fatal("Start() with a bad prefix = nil error, want error")
	}
}

func TestPollerCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newFixture()
	p := f.newPoller(t, false)

	if err := p.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestPollerLoopStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	f := newFixture()
	f.setCgroup(containerA, 0)
	p := f.newPoller(t, false)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := p.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	cancel()

	// Ranging to completion is the assertion: it only returns once the loop has
	// exited and closed the channel.
	for range events {
	}
}
