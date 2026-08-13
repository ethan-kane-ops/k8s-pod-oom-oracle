package detector

import (
	"context"
	"sync"
)

// Fake replays scripted kill events. It backs unit tests of the daemon
// pipeline and lets the e2e suite assert on a deterministic sequence without
// provoking a real kernel OOM.
type Fake struct {
	scripted []KillEvent

	mu        sync.Mutex
	events    chan KillEvent
	started   bool
	closeOnce sync.Once
}

// Compile-time check that Fake satisfies the interface.
var _ Detector = (*Fake)(nil)

// NewFake builds a detector that emits the given events on Start, in order.
func NewFake(events ...KillEvent) *Fake {
	return &Fake{
		scripted: events,
		// Buffered to the full script so Start never blocks.
		events: make(chan KillEvent, len(events)+1),
	}
}

// Source satisfies Detector.
func (f *Fake) Source() Source { return SourceFake }

// Start emits the scripted events and leaves the channel open for Emit.
func (f *Fake) Start(ctx context.Context) (<-chan KillEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.started {
		return f.events, nil
	}
	f.started = true

	for _, event := range f.scripted {
		select {
		case <-ctx.Done():
			return f.events, nil
		case f.events <- event:
		}
	}

	// Close the stream once the caller's context ends, matching the contract
	// real detectors follow.
	go func() {
		<-ctx.Done()
		_ = f.Close()
	}()

	return f.events, nil
}

// Emit publishes an additional event, letting a test drive the pipeline step by
// step after Start.
func (f *Fake) Emit(event KillEvent) {
	f.events <- event
}

// Close satisfies Detector. Safe to call more than once.
func (f *Fake) Close() error {
	f.closeOnce.Do(func() { close(f.events) })
	return nil
}
