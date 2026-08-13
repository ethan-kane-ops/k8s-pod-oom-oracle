package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// ptr returns a pointer to v, for the pointer-taking Put API.
func ptr[T any](v T) *T { return &v }

// report builds a minimal report with the given identity.
func report(id, namespace, pod, container string) oom.Report {
	return oom.Report{
		ID: id,
		Identity: correlate.Identity{
			Namespace: namespace, PodName: pod, ContainerName: container, Resolved: true,
		},
	}
}

func TestMemoryPutAndGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(10)

	want := report("a", "default", "api", "web")
	if err := s.Put(ctx, &want); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("Get() ID = %q, want %q", got.ID, want.ID)
	}
}

func TestMemoryGetMissing(t *testing.T) {
	t.Parallel()

	_, err := NewMemory(10).Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryPutReplacesSameID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(10)

	if err := s.Put(ctx, ptr(report("a", "default", "api", "web"))); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	if err := s.Put(ctx, ptr(report("a", "other", "api2", "web2"))); err != nil {
		t.Fatalf("second Put() error = %v", err)
	}

	if got := s.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1 after replacing the same ID", got)
	}
	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Identity.Namespace != "other" {
		t.Errorf("namespace = %q, want the replacement", got.Identity.Namespace)
	}
}

// TestMemoryEvictsOldest keeps a crash-looping workload from exhausting the
// daemon's own memory.
func TestMemoryEvictsOldest(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(3)

	for i := range 5 {
		if err := s.Put(ctx, ptr(report(strconv.Itoa(i), "default", "api", "web"))); err != nil {
			t.Fatalf("Put(%d) error = %v", i, err)
		}
	}

	if got := s.Len(); got != 3 {
		t.Fatalf("Len() = %d, want the capacity 3", got)
	}
	for _, evicted := range []string{"0", "1"} {
		if _, err := s.Get(ctx, evicted); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) error = %v, want the oldest evicted", evicted, err)
		}
	}
	for _, kept := range []string{"2", "3", "4"} {
		if _, err := s.Get(ctx, kept); err != nil {
			t.Errorf("Get(%q) error = %v, want it retained", kept, err)
		}
	}
}

func TestMemoryListNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(10)

	for _, id := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, ptr(report(id, "default", "api", "web"))); err != nil {
			t.Fatalf("Put(%q) error = %v", id, err)
		}
	}

	got, err := s.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(List()) = %d, want 3", len(got))
	}
	if got[0].ID != "c" || got[2].ID != "a" {
		t.Errorf("List() order = %s,%s,%s, want newest first", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestMemoryListFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(10)

	seed := []oom.Report{
		report("a", "default", "api", "web"),
		report("b", "default", "api", "sidecar"),
		report("c", "kube-system", "dns", "coredns"),
	}
	for i := range seed {
		if err := s.Put(ctx, &seed[i]); err != nil {
			t.Fatalf("Put(%q) error = %v", seed[i].ID, err)
		}
	}

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{name: "no filter", filter: Filter{}, want: []string{"c", "b", "a"}},
		{name: "by namespace", filter: Filter{Namespace: "default"}, want: []string{"b", "a"}},
		{name: "by pod", filter: Filter{Pod: "api"}, want: []string{"b", "a"}},
		{name: "by container", filter: Filter{Container: "web"}, want: []string{"a"}},
		{name: "combined", filter: Filter{Namespace: "default", Container: "sidecar"}, want: []string{"b"}},
		{name: "no matches", filter: Filter{Namespace: "missing"}, want: nil},
		{name: "limit applies after filtering", filter: Filter{Namespace: "default", Limit: 1}, want: []string{"b"}},
		{name: "limit larger than results", filter: Filter{Limit: 99}, want: []string{"c", "b", "a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := s.List(ctx, tt.filter)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(List()) = %d, want %d", len(got), len(tt.want))
			}
			for i, id := range tt.want {
				if got[i].ID != id {
					t.Errorf("List()[%d].ID = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestNewMemoryDefaultsCapacity(t *testing.T) {
	t.Parallel()

	for _, capacity := range []int{0, -1} {
		if got := NewMemory(capacity).capacity; got != DefaultCapacity {
			t.Errorf("NewMemory(%d).capacity = %d, want %d", capacity, got, DefaultCapacity)
		}
	}
}

func TestMemoryConcurrentAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := NewMemory(50)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := strconv.Itoa(i)
			if err := s.Put(ctx, ptr(report(id, "default", "api", "web"))); err != nil {
				t.Errorf("Put() error = %v", err)
			}
			_, _ = s.Get(ctx, id)
			_, _ = s.List(ctx, Filter{Limit: 5})
		}()
	}
	wg.Wait()

	if got := s.Len(); got != 50 {
		t.Errorf("Len() = %d, want 50", got)
	}
}
