package oom

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
)

// base is a fixed instant so trajectory assertions never depend on wall time.
var base = time.Date(2026, 8, 13, 8, 15, 0, 0, time.UTC)

func sample(offset time.Duration, current, limit uint64, stall float64) sampler.Sample {
	return sampler.Sample{
		Time:  base.Add(offset),
		Stats: cgroup.MemoryStats{Current: current, Limit: limit},
		PSI:   cgroup.PSI{Full: cgroup.PSILine{Avg10: stall}},
	}
}

func TestTrajectoryFrom(t *testing.T) {
	tests := []struct {
		name    string
		samples []sampler.Sample
		want    []TrajectoryPoint
	}{
		{
			name:    "no samples yields an empty trajectory",
			samples: nil,
			want:    []TrajectoryPoint{},
		},
		{
			name:    "usage and ratio are carried through",
			samples: []sampler.Sample{sample(0, 256, 512, 12.5)},
			want: []TrajectoryPoint{
				{Time: base, UsedBytes: 256, LimitBytes: 512, Ratio: 0.5, PressureFull: 12.5},
			},
		},
		{
			// A reader shown 18446744073709551615 learns nothing. The sentinel
			// is flattened so renderers can treat zero as "no ceiling".
			name:    "the unlimited sentinel is flattened to zero",
			samples: []sampler.Sample{sample(0, 256, cgroup.Unlimited, 0)},
			want: []TrajectoryPoint{
				{Time: base, UsedBytes: 256, LimitBytes: 0, Ratio: 0, PressureFull: 0},
			},
		},
		{
			name: "order is preserved oldest first",
			samples: []sampler.Sample{
				sample(0, 100, 1000, 1),
				sample(time.Second, 500, 1000, 20),
				sample(2*time.Second, 1000, 1000, 90),
			},
			want: []TrajectoryPoint{
				{Time: base, UsedBytes: 100, LimitBytes: 1000, Ratio: 0.1, PressureFull: 1},
				{Time: base.Add(time.Second), UsedBytes: 500, LimitBytes: 1000, Ratio: 0.5, PressureFull: 20},
				{Time: base.Add(2 * time.Second), UsedBytes: 1000, LimitBytes: 1000, Ratio: 1, PressureFull: 90},
			},
		},
		{
			// UsageRatio clamps, so a cgroup caught above its limit reports 1
			// rather than something greater that would overflow a progress bar.
			name:    "usage above the limit clamps the ratio at one",
			samples: []sampler.Sample{sample(0, 2000, 1000, 0)},
			want: []TrajectoryPoint{
				{Time: base, UsedBytes: 2000, LimitBytes: 1000, Ratio: 1, PressureFull: 0},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TrajectoryFrom(tc.samples)

			if got == nil {
				t.Fatal("TrajectoryFrom returned nil; callers marshal this directly and want [] not null")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d points, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("point %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestProcessesFrom(t *testing.T) {
	procs := []procfs.Process{
		{PID: 100, NSPid: 1, Comm: "server", Cmdline: []string{"node", "server.js"}, RSSBytes: 390},
		{PID: 200, NSPid: 17, Comm: "victim", Cmdline: []string{"tail", "/dev/zero"}, RSSBytes: 114},
		{PID: 300, NSPid: 20, Comm: "worker", RSSBytes: 8},
	}

	tests := []struct {
		name     string
		procs    []procfs.Process
		victim   detector.Victim
		wantPIDs []int
	}{
		{
			name:     "no processes yields no entries",
			procs:    nil,
			victim:   detector.Victim{PID: 200, NSPid: 17},
			wantPIDs: []int{},
		},
		{
			// A victim still visible in /proc while it dies must not be listed
			// alongside the processes that are genuinely there.
			name:     "the victim is excluded on a matching host pid",
			procs:    procs,
			victim:   detector.Victim{PID: 200, NSPid: 17},
			wantPIDs: []int{100, 300},
		},
		{
			// The defect this function exists to fix. Under a nested runtime the
			// probe reports the kernel's global pid, which is nowhere in the
			// daemon's /proc, so only NSPid can identify the victim.
			name:     "the victim is excluded on nspid when host pids disagree",
			procs:    procs,
			victim:   detector.Victim{PID: 1397320, NSPid: 17},
			wantPIDs: []int{100, 300},
		},
		{
			name:     "a host pid match wins even with a mismatched nspid",
			procs:    procs,
			victim:   detector.Victim{PID: 200, NSPid: 999},
			wantPIDs: []int{100, 300},
		},
		{
			name:     "an absent victim removes nothing",
			procs:    procs,
			victim:   detector.Victim{PID: 999, NSPid: 998},
			wantPIDs: []int{100, 200, 300},
		},
		{
			name:     "an unknown victim removes nothing",
			procs:    procs,
			victim:   detector.Victim{},
			wantPIDs: []int{100, 200, 300},
		},
		{
			// Deleting a process that is genuinely running is a worse report
			// than leaving the victim in, so an NSPid shared by two processes
			// disables the fallback instead of guessing between them.
			name: "an ambiguous nspid disables the fallback",
			procs: []procfs.Process{
				{PID: 100, NSPid: 1, Comm: "outer"},
				{PID: 200, NSPid: 1, Comm: "inner"},
			},
			victim:   detector.Victim{PID: 1397320, NSPid: 1},
			wantPIDs: []int{100, 200},
		},
		{
			name:     "a zero nspid on both sides is not a match",
			procs:    []procfs.Process{{PID: 100, NSPid: 0, Comm: "server"}},
			victim:   detector.Victim{PID: 1397320, NSPid: 0},
			wantPIDs: []int{100},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ProcessesFrom(tc.procs, tc.victim)

			if got == nil {
				t.Fatal("ProcessesFrom returned nil; callers marshal this directly and want [] not null")
			}
			if len(got) != len(tc.wantPIDs) {
				t.Fatalf("got %d processes, want %d", len(got), len(tc.wantPIDs))
			}
			for i, wantPID := range tc.wantPIDs {
				if got[i].PID != wantPID {
					t.Errorf("process %d has pid %d, want %d", i, got[i].PID, wantPID)
				}
			}
		})
	}
}

func TestProcessesFromCopiesEveryField(t *testing.T) {
	proc := procfs.Process{
		PID: 100, NSPid: 7, Comm: "server",
		Cmdline: []string{"node", "server.js"}, RSSBytes: 390,
	}

	got := ProcessesFrom([]procfs.Process{proc}, detector.Victim{})
	if len(got) != 1 {
		t.Fatalf("got %d processes, want 1", len(got))
	}

	snapshot := got[0]
	if snapshot.PID != proc.PID || snapshot.NSPid != proc.NSPid || snapshot.Comm != proc.Comm {
		t.Errorf("identity fields = %+v, want pid %d nspid %d comm %q",
			snapshot, proc.PID, proc.NSPid, proc.Comm)
	}
	if snapshot.RSSBytes != proc.RSSBytes {
		t.Errorf("RSSBytes = %d, want %d", snapshot.RSSBytes, proc.RSSBytes)
	}
	if len(snapshot.Cmdline) != len(proc.Cmdline) {
		t.Fatalf("Cmdline = %v, want %v", snapshot.Cmdline, proc.Cmdline)
	}
	for i := range proc.Cmdline {
		if snapshot.Cmdline[i] != proc.Cmdline[i] {
			t.Errorf("Cmdline[%d] = %q, want %q", i, snapshot.Cmdline[i], proc.Cmdline[i])
		}
	}
}

func TestReportPeakRatio(t *testing.T) {
	tests := []struct {
		name   string
		ratios []float64
		want   float64
	}{
		{name: "no trajectory has no peak", ratios: nil, want: 0},
		{name: "a single point is its own peak", ratios: []float64{0.42}, want: 0.42},
		{name: "the highest point wins when rising", ratios: []float64{0.1, 0.5, 0.9}, want: 0.9},
		{
			// The peak is not the last reading. A container that filled and was
			// then reclaimed still peaked at the value that got it killed.
			name:   "the highest point wins when it is not last",
			ratios: []float64{0.1, 0.97, 0.3},
			want:   0.97,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := Report{}
			for _, ratio := range tc.ratios {
				report.Trajectory = append(report.Trajectory, TrajectoryPoint{Ratio: ratio})
			}

			if got := report.PeakRatio(); got != tc.want {
				t.Errorf("PeakRatio() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReportWindow(t *testing.T) {
	tests := []struct {
		name    string
		offsets []time.Duration
		want    time.Duration
	}{
		{name: "no trajectory spans nothing", offsets: nil, want: 0},
		{
			// One reading is an instant, not a span. Reporting a duration here
			// would make a renderer print "over 0s" as though it measured one.
			name:    "a single point spans nothing",
			offsets: []time.Duration{0},
			want:    0,
		},
		{name: "two points span their gap", offsets: []time.Duration{0, 30 * time.Second}, want: 30 * time.Second},
		{
			name:    "many points span first to last",
			offsets: []time.Duration{0, 15 * time.Second, 45 * time.Second, time.Minute},
			want:    time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := Report{}
			for _, offset := range tc.offsets {
				report.Trajectory = append(report.Trajectory, TrajectoryPoint{Time: base.Add(offset)})
			}

			if got := report.Window(); got != tc.want {
				t.Errorf("Window() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGroupKillSerialisesAsATriState pins the wire contract the API reference
// promises. The field must be a nullable boolean, and an unknown must reach a
// consumer as an explicit null rather than being dropped from the object.
//
// `omitempty` would break this silently: a missing key and a false both decode
// to false in most clients, which is exactly the conflation the tri-state
// exists to prevent. A consumer that reads it as false reports that a container
// survived a kill nothing observed.
func TestGroupKillSerialisesAsATriState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value *bool
		want  string
	}{
		{name: "unknown is an explicit null", value: nil, want: `"groupKill":null`},
		{name: "group kill is true", value: ptr(true), want: `"groupKill":true`},
		{name: "single process kill is false", value: ptr(false), want: `"groupKill":false`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(Report{GroupKill: tt.value})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if !strings.Contains(string(encoded), tt.want) {
				t.Errorf("Marshal() = %s, want it to contain %s", encoded, tt.want)
			}
		})
	}
}

// ptr is the shortest way to build the pointer a tri-state field needs.
func ptr[T any](v T) *T { return &v }
