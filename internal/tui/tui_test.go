package tui

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/api"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Colour and timezone are pinned for the whole package.
//
// Both leak the developer's machine into a golden file. lipgloss decides on
// escape codes from the terminal it detects, and the list renders each report's
// time in local time, which is the right thing for an operator reading their own
// node and the wrong thing for a file compared byte for byte. Goldens written on
// a Mac in BST failed on a Linux runner in UTC by exactly one hour.
//
// The layout is what these assert on, and the layout is what has to survive a
// refactor.
func TestMain(m *testing.M) {
	// termenv.Ascii, not a bare zero: termenv numbers TrueColor as 0 and Ascii
	// as 3, so passing 0 here selects full colour and writes escape codes into
	// every golden file.
	lipgloss.SetColorProfile(termenv.Ascii)
	time.Local = time.UTC
	os.Exit(m.Run())
}

var testNow = time.Date(2026, 8, 13, 8, 15, 30, 0, time.UTC)

// fakeClient replays a scripted snapshot.
type fakeClient struct {
	snapshot Snapshot
	err      error
	calls    int
}

func (f *fakeClient) Fetch(context.Context) (Snapshot, error) {
	f.calls++
	if f.err != nil {
		return Snapshot{}, f.err
	}
	return f.snapshot, nil
}

func report(id, pod, container, victim string, at time.Time) oom.Report {
	const limit = 512 << 20

	// A real climb, not a flat line. The trajectory is the thing this tool has
	// that kubectl does not, so the fixture the golden files are built from has
	// to actually contain one.
	trajectory := make([]oom.TrajectoryPoint, 0, 5)
	for i, used := range []uint64{412 << 20, 460 << 20, 498 << 20, 507 << 20, limit} {
		trajectory = append(trajectory, oom.TrajectoryPoint{
			Time:         at.Add(time.Duration(i-4) * 15 * time.Second),
			UsedBytes:    used,
			LimitBytes:   limit,
			Ratio:        float64(used) / limit,
			PressureFull: float64(i) * 12,
		})
	}

	return oom.Report{
		ID:   id,
		Time: at,
		Identity: correlate.Identity{
			Scope:     correlate.Scope{PodUID: "3f0e2b6c-1a2b-4c3d-9e8f-0a1b2c3d4e5f", QoS: correlate.QoSBurstable},
			Namespace: "default", PodName: pod, ContainerName: container,
			Image: "payment:v1.2.0", Resolved: true,
		},
		Victim: detector.Victim{
			PID: 28145, NSPid: 17, Comm: victim,
			Cmdline: []string{victim, "./server.js"}, RSSBytes: 114 << 20, Known: true,
		},
		Source:      detector.SourceEBPF,
		KillCount:   1,
		LimitBytes:  limit,
		PeakBytes:   limit,
		Trajectory:  trajectory,
		VictimMatch: oom.VictimMatchHostPID,
		GroupKill:   ptr(true),
		Processes: []oom.ProcessSnapshot{
			{PID: 28102, NSPid: 1, Comm: "node", Cmdline: []string{"node", "./dist/server.js"}, RSSBytes: 390 << 20},
		},
		Trend: sampler.Trend{
			BytesPerSecond: 1 << 20, RSquared: 0.97, Samples: 5,
			Window: time.Minute, Projected: true,
		},
	}
}

func fullSnapshot() Snapshot {
	return Snapshot{
		Status: api.Status{
			Detector: "ebpf", CgroupVersion: "v2", Ready: true,
			Reports: 2, TrackedCgroups: 48, Node: "node-1",
			PodCacheSynced: true, PodsTracked: 12, Version: "v0.1.0",
		},
		Reports: []oom.Report{
			report("20260813T081522Z-2", "payment-api-6d5f78", "web-server", "node", testNow.Add(-8*time.Second)),
			report("20260813T081500Z-1", "worker-pool-99fabc", "worker", "python", testNow.Add(-30*time.Second)),
		},
	}
}

// newTestModel builds a model already sized and populated, as it is after the
// first window-size message and the first refresh.
func newTestModel(t *testing.T, client Client, width, height int) Model {
	t.Helper()

	m := New(Options{
		Client: client, Interval: time.Second, Addr: "http://127.0.0.1:9090",
		Now: func() time.Time { return testNow },
	})
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = next.(Model)

	snapshot, err := client.Fetch(context.Background())
	next, _ = m.Update(snapshotMsg{snapshot: snapshot, err: err, at: testNow.Add(-2 * time.Second)})
	return next.(Model)
}

func TestViewGolden(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		height int
		client Client
		mutate func(Model) Model
	}{
		{
			// The ordinary case: a wide terminal, both panes, a traced kill.
			name: "split", width: 170, height: 34,
			client: &fakeClient{snapshot: fullSnapshot()},
		},
		{
			// A healthy node. This is what the dashboard shows almost always,
			// so it has to read as "working", not as "broken".
			name: "no_reports", width: 170, height: 34,
			client: &fakeClient{snapshot: Snapshot{Status: api.Status{
				Detector: "ebpf", CgroupVersion: "v2", Ready: true, TrackedCgroups: 48, Node: "node-1",
			}}},
		},
		{
			// Every victim on screen is a guess, and the header has to say so.
			name: "poller_fallback", width: 170, height: 34,
			client: func() Client {
				s := fullSnapshot()
				s.Status.Detector = "poller"
				s.Reports[0].Victim.Inferred = true
				return &fakeClient{snapshot: s}
			}(),
		},
		{
			// Kills inside the kubepods tree that could not be placed. This is
			// the one counter worth alerting on, so it is coloured alone.
			name: "unattributed", width: 170, height: 34,
			client: func() Client {
				s := fullSnapshot()
				s.Status.Unattributed = 3
				s.Status.Skipped = 128
				return &fakeClient{snapshot: s}
			}(),
		},
		{
			// The daemon died. The reports already fetched stay on screen,
			// because that is when they matter most.
			name: "unreachable", width: 170, height: 34,
			client: &fakeClient{snapshot: fullSnapshot()},
			mutate: func(m Model) Model {
				next, _ := m.Update(snapshotMsg{err: errors.New("connection refused"), at: testNow})
				return next.(Model)
			},
		},
		{
			// An 80-column terminal. The report is fixed-width text, so the
			// panes stack rather than each taking half and wrapping.
			name: "narrow_list", width: 80, height: 24,
			client: &fakeClient{snapshot: fullSnapshot()},
		},
		{
			name: "narrow_detail", width: 80, height: 24,
			client: &fakeClient{snapshot: fullSnapshot()},
			mutate: func(m Model) Model {
				next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
				return next.(Model)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestModel(t, tt.client, tt.width, tt.height)
			if tt.mutate != nil {
				m = tt.mutate(m)
			}
			compareGolden(t, "view_"+tt.name+".txt", m.View())
		})
	}
}

// TestSelectionFollowsTheReportNotTheIndex is the bug this indexing exists to
// avoid. Reports arrive newest first, so a kill landing while someone reads an
// older one shifts every index down by one. An index-based selection would jump
// to a different report at exactly the wrong moment.
func TestSelectionFollowsTheReportNotTheIndex(t *testing.T) {
	client := &fakeClient{snapshot: fullSnapshot()}
	m := newTestModel(t, client, 170, 34)

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(Model)

	selected, ok := m.current()
	if !ok || selected.ID != "20260813T081500Z-1" {
		t.Fatalf("selected %q, want the second report", selected.ID)
	}

	// A newer kill arrives and takes the top of the list.
	grown := fullSnapshot()
	grown.Reports = append(
		[]oom.Report{report("20260813T081540Z-3", "new-pod", "app", "tail", testNow)},
		grown.Reports...,
	)
	next, _ = m.Update(snapshotMsg{snapshot: grown, at: testNow})
	m = next.(Model)

	after, ok := m.current()
	if !ok {
		t.Fatal("nothing selected after the refresh")
	}
	if after.ID != selected.ID {
		t.Errorf("selection moved to %q when a new report arrived, want it to stay on %q",
			after.ID, selected.ID)
	}
	if m.selected != 2 {
		t.Errorf("selected index = %d, want 2: the report moved down one when the new one landed", m.selected)
	}
}

// TestFailedRefreshKeepsTheLastSnapshot covers the moment the dashboard is most
// likely to be open. A node under memory pressure can take the daemon with it,
// and blanking the screen would discard the reports explaining why.
func TestFailedRefreshKeepsTheLastSnapshot(t *testing.T) {
	m := newTestModel(t, &fakeClient{snapshot: fullSnapshot()}, 170, 34)

	next, _ := m.Update(snapshotMsg{err: errors.New("connection refused"), at: testNow})
	m = next.(Model)

	if len(m.snapshot.Reports) != 2 {
		t.Errorf("reports = %d after a failed refresh, want the 2 already fetched",
			len(m.snapshot.Reports))
	}
	if m.err == nil {
		t.Error("the failure is not recorded, so nothing tells the reader the screen is stale")
	}
	if !strings.Contains(m.View(), "unreachable") {
		t.Error("the view does not say the daemon is unreachable")
	}
}

func TestMovementIsClamped(t *testing.T) {
	m := newTestModel(t, &fakeClient{snapshot: fullSnapshot()}, 170, 34)

	for range 10 {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = next.(Model)
	}
	if m.selected != 1 {
		t.Errorf("selected = %d after running off the end, want the last index 1", m.selected)
	}

	for range 10 {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		m = next.(Model)
	}
	if m.selected != 0 {
		t.Errorf("selected = %d after running off the top, want 0", m.selected)
	}
}

// TestMovementWithNoReportsDoesNotPanic is the empty state, which is the normal
// state of a healthy node rather than an edge case.
func TestMovementWithNoReportsDoesNotPanic(t *testing.T) {
	m := newTestModel(t, &fakeClient{snapshot: Snapshot{Status: api.Status{Detector: "ebpf"}}}, 170, 34)

	for _, key := range []string{"j", "k", "g", "G"} {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		m = next.(Model)
	}
	if _, ok := m.current(); ok {
		t.Error("current() returned a report from an empty list")
	}
}

func TestQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		m := newTestModel(t, &fakeClient{snapshot: fullSnapshot()}, 170, 34)
		next, cmd := m.Update(key)
		if cmd == nil {
			t.Errorf("%s produced no command, want tea.Quit", key)
		}
		if !next.(Model).quitting {
			t.Errorf("%s did not mark the model as quitting", key)
		}
	}
}

// TestRefreshKeyFetches checks that r reaches the client rather than only
// redrawing what is already on screen.
func TestRefreshKeyFetches(t *testing.T) {
	client := &fakeClient{snapshot: fullSnapshot()}
	m := newTestModel(t, client, 170, 34)
	before := client.calls

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r produced no command")
	}
	cmd()

	if client.calls != before+1 {
		t.Errorf("client calls = %d, want %d: r must re-read the daemon", client.calls, before+1)
	}
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()

	// Trailing whitespace is invisible in a diff and varies with how lipgloss
	// pads a line, so it is stripped from both sides rather than pinned.
	got = trimTrailing(got)

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
		t.Fatalf("reading golden file (run `go test ./internal/tui -update` to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func trimTrailing(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
}

// ptr is the shortest way to build the pointer a tri-state field needs.
func ptr[T any](v T) *T { return &v }
