// Package sampler keeps a rolling memory history per cgroup and derives the
// growth trend that precedes an OOM kill.
//
// The daemon cannot ask the kernel what memory looked like before a kill, so it
// has to have been watching. Each cgroup gets a fixed-size ring of samples;
// when a kill arrives, the ring is the post-mortem trajectory.
package sampler

import (
	"fmt"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

// Sample is one timestamped memory reading for a cgroup.
type Sample struct {
	Time  time.Time          `json:"time"`
	Stats cgroup.MemoryStats `json:"stats"`
	PSI   cgroup.PSI         `json:"psi"`
}

// Ring is a fixed-capacity circular buffer of samples. It is not safe for
// concurrent use; Sampler owns the locking.
type Ring struct {
	samples []Sample
	next    int
	filled  bool
}

// NewRing allocates a ring holding up to capacity samples.
//
// A non-positive capacity is a programmer error, not a runtime condition, and
// panics. Every construction path validates the capacity first.
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		panic(fmt.Sprintf("sampler: ring capacity must be positive, got %d", capacity))
	}
	return &Ring{samples: make([]Sample, capacity)}
}

// Add appends a sample, overwriting the oldest once the ring is full.
func (r *Ring) Add(s Sample) {
	r.samples[r.next] = s
	r.next = (r.next + 1) % len(r.samples)
	if r.next == 0 {
		r.filled = true
	}
}

// Len reports how many samples the ring currently holds.
func (r *Ring) Len() int {
	if r.filled {
		return len(r.samples)
	}
	return r.next
}

// Cap reports the ring's capacity.
func (r *Ring) Cap() int { return len(r.samples) }

// Samples returns a copy of the buffered samples, oldest first. The copy means
// callers can hold the result after the ring has moved on.
func (r *Ring) Samples() []Sample {
	out := make([]Sample, 0, r.Len())
	if r.filled {
		out = append(out, r.samples[r.next:]...)
	}
	return append(out, r.samples[:r.next]...)
}

// Latest returns the most recent sample, reporting false when the ring is empty.
func (r *Ring) Latest() (Sample, bool) {
	if r.Len() == 0 {
		return Sample{}, false
	}
	index := (r.next - 1 + len(r.samples)) % len(r.samples)
	return r.samples[index], true
}
