package render

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
)

var update = flag.Bool("update", false, "rewrite golden files")

var reportTime = time.Date(2026, 8, 13, 8, 15, 22, 0, time.UTC)

// fullReport is the happy path: everything known, trajectory buffered.
func fullReport() oom.Report {
	const limit = 512 << 20

	trajectory := make([]oom.TrajectoryPoint, 0, 5)
	for i, used := range []uint64{412 << 20, 460 << 20, 498 << 20, 507 << 20, 512 << 20} {
		trajectory = append(trajectory, oom.TrajectoryPoint{
			Time:         reportTime.Add(time.Duration(i-4) * 15 * time.Second),
			UsedBytes:    used,
			LimitBytes:   limit,
			Ratio:        float64(used) / limit,
			PressureFull: float64(i) * 12,
		})
	}

	return oom.Report{
		ID:   "20260813T081522Z-1",
		Time: reportTime,
		Identity: correlate.Identity{
			Scope: correlate.Scope{
				PodUID:      "3f0e2b6c-1a2b-4c3d-9e8f-0a1b2c3d4e5f",
				ContainerID: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d",
				QoS:         correlate.QoSBurstable,
			},
			Namespace: "default", PodName: "payment-api-6d5f78",
			ContainerName: "web-server", Image: "payment:v1.2.0", Resolved: true,
		},
		Victim: detector.Victim{
			PID: 28145, NSPid: 17, Comm: "node",
			Cmdline:  []string{"node", "./dist/garbage-collector.js"},
			RSSBytes: 114 << 20, Inferred: false, Known: true,
		},
		Source:     detector.SourceEBPF,
		KillCount:  1,
		LimitBytes: limit,
		PeakBytes:  limit,
		Trajectory: trajectory,
		Trend: sampler.Trend{
			BytesPerSecond: 1 << 20, RSquared: 0.97, Samples: 5,
			Window: time.Minute, TimeToLimit: 0, Projected: true,
		},
		Processes: []oom.ProcessSnapshot{
			{PID: 28102, NSPid: 1, Comm: "node", Cmdline: []string{"node", "./dist/server.js"}, RSSBytes: 390 << 20},
			{PID: 28160, NSPid: 22, Comm: "node", Cmdline: []string{"node", "./dist/worker.js"}, RSSBytes: 8 << 20},
		},
	}
}

func TestTextGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report func() oom.Report
	}{
		{name: "full", report: fullReport},
		{
			name: "inferred_victim",
			report: func() oom.Report {
				r := fullReport()
				r.Source = detector.SourcePoller
				r.Victim.Inferred = true
				return r
			},
		},
		{
			name: "unknown_victim",
			report: func() oom.Report {
				r := fullReport()
				r.Source = detector.SourcePoller
				r.Victim = detector.Victim{}
				return r
			},
		},
		{
			name: "unresolved_pod",
			report: func() oom.Report {
				r := fullReport()
				r.Identity.Resolved = false
				r.Identity.Namespace = ""
				r.Identity.PodName = ""
				r.Identity.ContainerName = ""
				r.Identity.Image = ""
				return r
			},
		},
		{
			name: "no_trajectory",
			report: func() oom.Report {
				r := fullReport()
				r.Trajectory = nil
				r.Trend = sampler.Trend{}
				return r
			},
		},
		{
			name: "no_processes",
			report: func() oom.Report {
				r := fullReport()
				r.Processes = nil
				return r
			},
		},
		{
			// The containerd default. The listing must not be presented as
			// survivors, because none of them are.
			name: "group_kill",
			report: func() oom.Report {
				r := fullReport()
				r.GroupKill = true
				return r
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := tt.report()
			got := Text(&report, TextOptions{})
			compareGolden(t, "text_"+tt.name+".txt", got)
		})
	}
}

// compareGolden checks output against a committed fixture, rewriting it when
// -update is passed.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run `go test ./internal/render -update` to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bytes uint64
		want  string
	}{
		{bytes: 0, want: "0B"},
		{bytes: 512, want: "512B"},
		{bytes: 1023, want: "1023B"},
		{bytes: 1024, want: "1.0KiB"},
		{bytes: 1536, want: "1.5KiB"},
		{bytes: 1 << 20, want: "1.0MiB"},
		{bytes: 512 << 20, want: "512.0MiB"},
		{bytes: 1 << 30, want: "1.0GiB"},
		{bytes: 1 << 40, want: "1.0TiB"},
		{bytes: 1 << 50, want: "1.0PiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := Bytes(tt.bytes); got != tt.want {
				t.Errorf("Bytes(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestBar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ratio      float64
		wantFilled int
	}{
		{name: "empty", ratio: 0, wantFilled: 0},
		{name: "half", ratio: 0.5, wantFilled: 9},
		{name: "full", ratio: 1, wantFilled: barWidth},
		{name: "just under full leaves a gap", ratio: 0.99, wantFilled: barWidth - 1},
		{name: "negative clamps to empty", ratio: -1, wantFilled: 0},
		{name: "over one clamps to full", ratio: 2, wantFilled: barWidth},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := bar(tt.ratio)
			if filled := strings.Count(got, "█"); filled != tt.wantFilled {
				t.Errorf("bar(%v) filled = %d, want %d", tt.ratio, filled, tt.wantFilled)
			}
			// The meter must stay a fixed width regardless of ratio.
			if total := strings.Count(got, "█") + strings.Count(got, "░"); total != barWidth {
				t.Errorf("bar(%v) width = %d, want %d", tt.ratio, total, barWidth)
			}
		})
	}
}

func TestDownsample(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []int
		limit int
		want  []int
	}{
		{name: "under the limit is untouched", input: []int{1, 2, 3}, limit: 5, want: []int{1, 2, 3}},
		{name: "exactly at the limit", input: []int{1, 2, 3}, limit: 3, want: []int{1, 2, 3}},
		{name: "evenly spaced", input: []int{1, 2, 3, 4, 5, 6, 7, 8, 9}, limit: 3, want: []int{1, 5, 9}},
		{name: "keeps first and last", input: []int{1, 2, 3, 4, 5, 6, 7}, limit: 4, want: []int{1, 3, 5, 7}},
		{name: "zero limit is untouched", input: []int{1, 2, 3}, limit: 0, want: []int{1, 2, 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := downsample(tt.input, tt.limit)
			if len(got) != len(tt.want) {
				t.Fatalf("downsample() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("downsample() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestTextTruncatesLongProcessList keeps a container with hundreds of processes
// from flooding a terminal.
func TestTextTruncatesLongProcessList(t *testing.T) {
	t.Parallel()

	report := fullReport()
	report.Processes = make([]oom.ProcessSnapshot, 25)
	for i := range report.Processes {
		report.Processes[i] = oom.ProcessSnapshot{PID: 1000 + i, Comm: "worker", RSSBytes: uint64(i) << 20}
	}

	got := Text(&report, TextOptions{})
	if !strings.Contains(got, "and 15 more") {
		t.Errorf("output does not mention the truncated processes:\n%s", got)
	}
}

func TestJSONRoundTrips(t *testing.T) {
	t.Parallel()

	report := fullReport()
	payload, err := JSON(&report)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	var round oom.Report
	if err := json.Unmarshal(payload, &round); err != nil {
		t.Fatalf("unmarshaling JSON() output: %v", err)
	}
	if round.ID != "20260813T081522Z-1" {
		t.Errorf("ID = %q, want it preserved", round.ID)
	}
	if round.Victim.PID != 28145 {
		t.Errorf("Victim.PID = %d, want it preserved", round.Victim.PID)
	}
	if len(round.Trajectory) != 5 {
		t.Errorf("len(Trajectory) = %d, want 5", len(round.Trajectory))
	}
}

// TestJSONListRendersEmptyAsArray keeps consumers from having to nil-check.
func TestJSONListRendersEmptyAsArray(t *testing.T) {
	t.Parallel()

	payload, err := JSONList(nil)
	if err != nil {
		t.Fatalf("JSONList() error = %v", err)
	}
	if got := strings.TrimSpace(string(payload)); got != "[]" {
		t.Errorf("JSONList(nil) = %q, want %q", got, "[]")
	}
}

// TestTrajectoryRowsAlwaysKeepsThePeak guards the failure mode that made a real
// OOM report render as a flat line: a container idle for most of its history
// that balloons in under a second. Evenly spaced rows miss the spike entirely.
func TestTrajectoryRowsAlwaysKeepsThePeak(t *testing.T) {
	t.Parallel()

	// 40 idle readings, one spike near the end, then a post-kill drop.
	points := make([]oom.TrajectoryPoint, 0, 42)
	for i := range 40 {
		points = append(points, oom.TrajectoryPoint{
			Time: reportTime.Add(time.Duration(i) * time.Second), UsedBytes: 736 << 10,
		})
	}
	points = append(points,
		oom.TrajectoryPoint{Time: reportTime.Add(40 * time.Second), UsedBytes: 512 << 20},
		oom.TrajectoryPoint{Time: reportTime.Add(41 * time.Second), UsedBytes: 732 << 10},
	)

	rows := trajectoryRows(points, maxTrajectoryRows)

	if len(rows) != maxTrajectoryRows {
		t.Fatalf("len(rows) = %d, want %d", len(rows), maxTrajectoryRows)
	}

	var sawPeak bool
	for _, row := range rows {
		if row.UsedBytes == 512<<20 {
			sawPeak = true
		}
	}
	if !sawPeak {
		t.Error("the peak reading was dropped; the report would show a flat line for an OOM kill")
	}

	// Rows must stay in time order, and the window anchors must survive.
	for i := 1; i < len(rows); i++ {
		if rows[i].Time.Before(rows[i-1].Time) {
			t.Fatalf("rows are not in time order at index %d", i)
		}
	}
	if !rows[0].Time.Equal(points[0].Time) {
		t.Error("first reading was displaced")
	}
	if !rows[len(rows)-1].Time.Equal(points[len(points)-1].Time) {
		t.Error("last reading was displaced")
	}
}

func TestCommandLineFlattensAndTruncates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		comm    string
		cmdline []string
		want    string
	}{
		{name: "simple", cmdline: []string{"node", "server.js"}, want: "node server.js"},
		{name: "falls back to comm", comm: "kworker/0:1", want: "kworker/0:1"},
		{
			name:    "inline shell script is flattened onto one line",
			cmdline: []string{"sh", "-c", "\n  sleep 600 &\n  tail /dev/zero\n"},
			want:    "sh -c sleep 600 & tail /dev/zero",
		},
		{
			name:    "tabs and control characters collapse",
			cmdline: []string{"app", "\t--flag\x00value"},
			want:    "app --flag value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := commandLine(tt.comm, tt.cmdline); got != tt.want {
				t.Errorf("commandLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommandLineTruncatesLongArguments(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 500)
	got := commandLine("", []string{"java", long})

	if len([]rune(got)) != maxCommandLength {
		t.Errorf("len(commandLine()) = %d runes, want %d", len([]rune(got)), maxCommandLength)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("commandLine() = %q, want a truncation marker", got)
	}
}

// TestTextNeverEmitsControlCharacters keeps a hostile or merely messy command
// line from corrupting a terminal.
func TestTextNeverEmitsControlCharacters(t *testing.T) {
	t.Parallel()

	report := fullReport()
	report.Victim.Cmdline = []string{"sh", "-c", "echo\n\r\x1b[31mhax\x1b[0m"}
	report.Processes = []oom.ProcessSnapshot{{PID: 1, Comm: "x", Cmdline: []string{"a\nb"}, RSSBytes: 1}}

	for _, r := range Text(&report, TextOptions{}) {
		if r != '\n' && unicode.IsControl(r) {
			t.Fatalf("output contains control character %q", r)
		}
	}
}
