package tui

import (
	"context"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestLiveDaemonSmoke drives the real client and model against a running
// daemon. Skipped unless OOM_ORACLE_LIVE names one.
func TestLiveDaemonSmoke(t *testing.T) {
	addr := os.Getenv("OOM_ORACLE_LIVE")
	if addr == "" {
		t.Skip("set OOM_ORACLE_LIVE to a daemon base URL to run this")
	}

	client := &HTTPClient{Addr: addr}
	snapshot, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	t.Logf("live: detector=%s reports=%d tracking=%d node=%s",
		snapshot.Status.Detector, len(snapshot.Reports),
		snapshot.Status.TrackedCgroups, snapshot.Status.Node)

	m := New(Options{Client: client, Interval: time.Second, Addr: addr})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 170, Height: 34})
	next, _ = next.(Model).Update(snapshotMsg{snapshot: snapshot, at: time.Now()})
	view := next.(Model).View()
	if view == "" {
		t.Fatal("View() rendered nothing against a live daemon")
	}
	t.Logf("rendered %d bytes\n%s", len(view), view)
}
