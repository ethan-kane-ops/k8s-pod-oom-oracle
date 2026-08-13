// Package store retains post-mortem reports for later retrieval.
package store

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// ErrNotFound is returned when a report ID is unknown.
var ErrNotFound = errors.New("report not found")

// Filter narrows a listing. Zero-valued fields are ignored.
type Filter struct {
	Namespace string
	Pod       string
	Container string
	// Limit caps the number of reports returned, newest first. Zero means all.
	Limit int
}

// matches reports whether a report satisfies the filter.
func (f Filter) matches(r *oom.Report) bool {
	if f.Namespace != "" && r.Identity.Namespace != f.Namespace {
		return false
	}
	if f.Pod != "" && r.Identity.PodName != f.Pod {
		return false
	}
	if f.Container != "" && r.Identity.ContainerName != f.Container {
		return false
	}
	return true
}

// Store retains reports. Implementations must be safe for concurrent use.
type Store interface {
	// Put records a report. The store copies it, so the caller may reuse the
	// value afterwards.
	Put(ctx context.Context, report *oom.Report) error
	// Get retrieves one report by ID, returning ErrNotFound if absent.
	Get(ctx context.Context, id string) (oom.Report, error)
	// List returns matching reports, newest first.
	List(ctx context.Context, filter Filter) ([]oom.Report, error)
}

// DefaultCapacity bounds the in-memory store. A node producing more OOM kills
// than this in one daemon lifetime has bigger problems than lost history.
const DefaultCapacity = 256

// Memory is a bounded in-memory Store. Oldest reports are dropped once the
// capacity is reached, which keeps a crash-looping workload from exhausting the
// daemon's own memory. Reports do not survive a restart.
type Memory struct {
	capacity int

	mu sync.RWMutex
	// ordered holds reports oldest first.
	ordered []oom.Report
	byID    map[string]int
}

// Compile-time check that Memory satisfies the interface.
var _ Store = (*Memory)(nil)

// NewMemory builds an in-memory store. A non-positive capacity uses the default.
func NewMemory(capacity int) *Memory {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Memory{
		capacity: capacity,
		ordered:  make([]oom.Report, 0, capacity),
		byID:     make(map[string]int, capacity),
	}
}

// Put records a report, evicting the oldest once at capacity.
func (m *Memory) Put(_ context.Context, report *oom.Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if index, ok := m.byID[report.ID]; ok {
		m.ordered[index] = *report
		return nil
	}

	if len(m.ordered) >= m.capacity {
		m.ordered = m.ordered[1:]
		m.reindexLocked()
	}

	m.ordered = append(m.ordered, *report)
	m.byID[report.ID] = len(m.ordered) - 1

	return nil
}

// reindexLocked rebuilds the ID index after the slice shifts.
func (m *Memory) reindexLocked() {
	clear(m.byID)
	for i := range m.ordered {
		m.byID[m.ordered[i].ID] = i
	}
}

// Get retrieves one report by ID.
func (m *Memory) Get(_ context.Context, id string) (oom.Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	index, ok := m.byID[id]
	if !ok {
		return oom.Report{}, ErrNotFound
	}
	return m.ordered[index], nil
}

// List returns matching reports, newest first.
func (m *Memory) List(_ context.Context, filter Filter) ([]oom.Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	matched := make([]oom.Report, 0, len(m.ordered))
	for i := range m.ordered {
		if filter.matches(&m.ordered[i]) {
			matched = append(matched, m.ordered[i])
		}
	}
	slices.Reverse(matched)

	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return matched, nil
}

// Len reports how many reports are held.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.ordered)
}
