package sampler

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

var epoch = time.Date(2026, 8, 13, 8, 0, 0, 0, time.UTC)

// sampleAt builds a sample offsetSeconds after the epoch with the given usage.
func sampleAt(offsetSeconds int, current uint64) Sample {
	return Sample{
		Time:  epoch.Add(time.Duration(offsetSeconds) * time.Second),
		Stats: cgroup.MemoryStats{Current: current, Limit: 1 << 30},
	}
}

// currents extracts the usage series so tests can assert ordering compactly.
func currents(samples []Sample) []uint64 {
	out := make([]uint64, len(samples))
	for i, s := range samples {
		out[i] = s.Stats.Current
	}
	return out
}

func TestNewRingPanicsOnNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1} {
		t.Run(strconv.Itoa(capacity), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("NewRing(%d) did not panic", capacity)
				}
			}()
			NewRing(capacity)
		})
	}
}

func TestRingOrdersOldestFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capacity int
		added    int
		want     []uint64
	}{
		{name: "empty", capacity: 3, added: 0, want: []uint64{}},
		{name: "partially filled", capacity: 5, added: 3, want: []uint64{0, 1, 2}},
		{name: "exactly full", capacity: 3, added: 3, want: []uint64{0, 1, 2}},
		{name: "wrapped once", capacity: 3, added: 5, want: []uint64{2, 3, 4}},
		{name: "wrapped several times", capacity: 3, added: 10, want: []uint64{7, 8, 9}},
		{name: "capacity one keeps only the newest", capacity: 1, added: 4, want: []uint64{3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ring := NewRing(tt.capacity)
			for i := range tt.added {
				ring.Add(sampleAt(i, uint64(i)))
			}

			if got := currents(ring.Samples()); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Samples() = %v, want %v", got, tt.want)
			}
			if got, want := ring.Len(), len(tt.want); got != want {
				t.Errorf("Len() = %d, want %d", got, want)
			}
			if got := ring.Cap(); got != tt.capacity {
				t.Errorf("Cap() = %d, want %d", got, tt.capacity)
			}
		})
	}
}

func TestRingLatest(t *testing.T) {
	t.Parallel()

	ring := NewRing(3)

	if _, ok := ring.Latest(); ok {
		t.Error("Latest() on an empty ring reported ok")
	}

	for i := range 5 {
		ring.Add(sampleAt(i, uint64(i*100)))
	}

	latest, ok := ring.Latest()
	if !ok {
		t.Fatal("Latest() reported not ok on a populated ring")
	}
	if latest.Stats.Current != 400 {
		t.Errorf("Latest().Stats.Current = %d, want 400", latest.Stats.Current)
	}
}

func TestRingSamplesIsACopy(t *testing.T) {
	t.Parallel()

	ring := NewRing(2)
	ring.Add(sampleAt(0, 100))
	ring.Add(sampleAt(1, 200))

	snapshot := ring.Samples()

	// Overwrite the whole ring; the earlier snapshot must be unaffected.
	ring.Add(sampleAt(2, 300))
	ring.Add(sampleAt(3, 400))

	if got := currents(snapshot); !reflect.DeepEqual(got, []uint64{100, 200}) {
		t.Errorf("snapshot mutated to %v; Samples() must return a copy", got)
	}
}
