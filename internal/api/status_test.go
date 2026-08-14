package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

// errStore fails every operation, standing in for a store whose backing
// resource has gone away. The handlers must answer 500 rather than panic or
// serve a half-written body.
type errStore struct{ err error }

func (s errStore) Put(context.Context, *oom.Report) error { return s.err }

func (s errStore) Get(context.Context, string) (oom.Report, error) { return oom.Report{}, s.err }

func (s errStore) List(context.Context, store.Filter) ([]oom.Report, error) { return nil, s.err }

// failingWriter accepts headers but refuses the body, which is what a client
// that hangs up mid-response looks like to a handler.
type failingWriter struct {
	header http.Header
	code   int
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

func (w *failingWriter) WriteHeader(code int) { w.code = code }

func newServer(t *testing.T, opts Options) *Server {
	t.Helper()

	if opts.Store == nil {
		opts.Store = store.NewMemory(1)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return s
}

func TestStatusServesTheSnapshot(t *testing.T) {
	t.Parallel()

	want := Status{
		Detector:       "ebpf",
		CgroupVersion:  "v2",
		Ready:          true,
		Reports:        7,
		Skipped:        2,
		TrackedCgroups: 47,
		UptimeSeconds:  12.5,
		Version:        "v1.2.3",
		Node:           "worker-1",
		PodCacheSynced: true,
		PodsTracked:    10,
	}

	s := newServer(t, Options{Status: func() Status { return want }})
	rec := get(t, s.Handler(), "/v1/status")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding status: %v (body %s)", err, rec.Body)
	}
	if got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

// The daemon reports cluster correlation through this endpoint and nowhere
// else, so the three fields the e2e suite asserts on must survive marshalling
// under their documented names.
func TestStatusFieldNames(t *testing.T) {
	t.Parallel()

	s := newServer(t, Options{Status: func() Status {
		return Status{Node: "worker-1", PodCacheSynced: true, PodsTracked: 10}
	}})
	rec := get(t, s.Handler(), "/v1/status")

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding status: %v", err)
	}

	for _, key := range []string{"detector", "cgroupVersion", "ready", "node", "podCacheSynced", "podsTracked"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("status response has no %q field; keys are %v", key, raw)
		}
	}
}

// Node is omitempty, so a daemon running with --kubernetes=off must omit it
// rather than advertise an empty node name.
func TestStatusOmitsNodeWhenCorrelationIsOff(t *testing.T) {
	t.Parallel()

	s := newServer(t, Options{Status: func() Status { return Status{Detector: "poller"} }})
	rec := get(t, s.Handler(), "/v1/status")

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if _, ok := raw["node"]; ok {
		t.Errorf("status carries a node field with correlation off: %v", raw)
	}
}

// With no Status function the server still has to answer, because /v1/status
// is what an operator hits first when something looks wrong.
func TestStatusDefaultsToReadiness(t *testing.T) {
	t.Parallel()

	s := newServer(t, Options{Ready: func() bool { return false }})
	rec := get(t, s.Handler(), "/v1/status")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if got.Ready {
		t.Error("default status reports ready while the readiness function says otherwise")
	}
}

func TestStoreFailuresBecomeServerErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
	}{
		{name: "listing", target: "/v1/events"},
		{name: "getting one", target: "/v1/events/a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newServer(t, Options{Store: errStore{err: errors.New("backing store gone")}})
			rec := get(t, s.Handler(), tt.target)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding error response: %v (body %s)", err, rec.Body)
			}
			if body["error"] == "" {
				t.Error("error response carries no message")
			}
			// The underlying error is logged, never served. It can name paths
			// on the node, and this endpoint is reachable from the cluster.
			if body["error"] == "backing store gone" {
				t.Error("internal error text leaked to the client")
			}
		})
	}
}

// A client that disappears mid-write must not take the daemon with it. These
// paths only log, so the assertion is that they return at all.
func TestWritesSurviveABrokenConnection(t *testing.T) {
	t.Parallel()

	s := newServer(t, Options{Status: func() Status { return Status{Detector: "ebpf"} }})
	handler := s.Handler()

	for _, target := range []string{"/healthz", "/readyz", "/v1/status", "/v1/events", "/v1/events/missing"} {
		w := &failingWriter{}
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, http.NoBody))
		if w.code == 0 {
			t.Errorf("GET %s never wrote a status header", target)
		}
	}
}

// A listen failure has to surface. Binding an address already in use is the
// realistic case: two daemons on one node, or a leftover process.
func TestListenAndServeReportsABindFailure(t *testing.T) {
	t.Parallel()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	s := newServer(t, Options{Addr: occupied.Addr().String()})

	if err := s.ListenAndServe(context.Background()); err == nil {
		t.Fatal("ListenAndServe() on an occupied address = nil error, want an error")
	}
}
