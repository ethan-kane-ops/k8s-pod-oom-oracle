package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

// newTestServer builds a server over a store seeded with three reports.
func newTestServer(t *testing.T, ready func() bool) http.Handler {
	t.Helper()

	reports := store.NewMemory(10)
	seed := []oom.Report{
		{ID: "a", Identity: correlate.Identity{Namespace: "default", PodName: "api", ContainerName: "web", Resolved: true}},
		{ID: "b", Identity: correlate.Identity{Namespace: "default", PodName: "api", ContainerName: "sidecar", Resolved: true}},
		{ID: "c", Identity: correlate.Identity{Namespace: "kube-system", PodName: "dns", ContainerName: "coredns", Resolved: true}},
	}
	for i := range seed {
		if err := reports.Put(context.Background(), &seed[i]); err != nil {
			t.Fatalf("seeding store: %v", err)
		}
	}

	s, err := New(Options{Store: reports, Ready: ready})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s.Handler()
}

// get performs a request against the handler.
func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, http.NoBody))
	return rec
}

func TestNewRequiresStore(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{}); err == nil {
		t.Fatal("New() without a store = nil error, want error")
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestServer(t, nil), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ready func() bool
		want  int
	}{
		{name: "default is ready", ready: nil, want: http.StatusOK},
		{name: "ready", ready: func() bool { return true }, want: http.StatusOK},
		{name: "not ready", ready: func() bool { return false }, want: http.StatusServiceUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := get(t, newTestServer(t, tt.ready), "/readyz")
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestListEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{name: "all newest first", target: "/v1/events", want: []string{"c", "b", "a"}},
		{name: "by namespace", target: "/v1/events?namespace=default", want: []string{"b", "a"}},
		{name: "by pod", target: "/v1/events?pod=api", want: []string{"b", "a"}},
		{name: "by container", target: "/v1/events?container=web", want: []string{"a"}},
		{name: "limited", target: "/v1/events?limit=2", want: []string{"c", "b"}},
		{name: "combined", target: "/v1/events?namespace=default&container=sidecar", want: []string{"b"}},
		{name: "no matches", target: "/v1/events?namespace=nowhere", want: []string{}},
	}

	handler := newTestServer(t, nil)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := get(t, handler, tt.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var got []oom.Report
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding response: %v (body %s)", err, rec.Body)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d reports, want %d", len(got), len(tt.want))
			}
			for i, id := range tt.want {
				if got[i].ID != id {
					t.Errorf("report[%d].ID = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestListEventsRejectsBadLimit(t *testing.T) {
	t.Parallel()

	handler := newTestServer(t, nil)

	for _, target := range []string{"/v1/events?limit=abc", "/v1/events?limit=-1"} {
		rec := get(t, handler, target)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestGetEvent(t *testing.T) {
	t.Parallel()

	handler := newTestServer(t, nil)

	rec := get(t, handler, "/v1/events/b")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got oom.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("ID = %q, want %q", got.ID, "b")
	}
}

func TestGetEventNotFound(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestServer(t, nil), "/v1/events/missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if body["error"] == "" {
		t.Error("error response carries no message")
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	t.Parallel()

	rec := get(t, newTestServer(t, nil), "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestListenAndServeShutsDownOnCancel(t *testing.T) {
	t.Parallel()

	s, err := New(Options{Addr: "127.0.0.1:0", Store: store.NewMemory(1)})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe() error = %v, want nil on cancellation", err)
	}
}
