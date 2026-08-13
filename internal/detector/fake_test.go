package detector

import (
	"context"
	"testing"
)

func TestFakeReplaysScriptedEvents(t *testing.T) {
	t.Parallel()

	scripted := []KillEvent{
		{CgroupPath: containerA, KillCount: 1, Source: SourceFake},
		{CgroupPath: containerB, KillCount: 1, Source: SourceFake},
	}

	fake := NewFake(scripted...)
	if got := fake.Source(); got != SourceFake {
		t.Errorf("Source() = %q, want %q", got, SourceFake)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := fake.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	got := drain(t, events)
	if len(got) != len(scripted) {
		t.Fatalf("received %d events, want %d", len(got), len(scripted))
	}
	for i := range scripted {
		if got[i].CgroupPath != scripted[i].CgroupPath {
			t.Errorf("event %d cgroup = %q, want %q", i, got[i].CgroupPath, scripted[i].CgroupPath)
		}
	}
}

func TestFakeEmitAfterStart(t *testing.T) {
	t.Parallel()

	fake := NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events, err := fake.Start(ctx)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	fake.Emit(KillEvent{CgroupPath: containerA, KillCount: 7})

	got := <-events
	if got.CgroupPath != containerA || got.KillCount != 7 {
		t.Errorf("Emit() delivered %+v, want the emitted event", got)
	}
}

func TestFakeStartIsIdempotent(t *testing.T) {
	t.Parallel()

	fake := NewFake(KillEvent{CgroupPath: containerA})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := fake.Start(ctx)
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	second, err := fake.Start(ctx)
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	if len(drain(t, first)) != 1 {
		t.Error("first Start() did not deliver the scripted event")
	}
	if len(drain(t, second)) != 0 {
		t.Error("second Start() replayed the script")
	}
}

func TestFakeCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	fake := NewFake()
	if err := fake.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := fake.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestDetectorsSatisfyInterface keeps the implementations interchangeable,
// which is the whole point of the abstraction.
func TestDetectorsSatisfyInterface(t *testing.T) {
	t.Parallel()

	f := newFixture()
	detectors := map[string]Detector{
		"fake":   NewFake(),
		"poller": f.newPoller(t, false),
	}

	for name, d := range detectors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if d.Source() == "" {
				t.Error("Source() is empty")
			}
			if err := d.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})
	}
}
