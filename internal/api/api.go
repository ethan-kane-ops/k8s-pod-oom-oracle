// Package api serves reports and health over HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/render"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

// Timeouts guard against slow or stuck clients holding daemon resources.
const (
	readHeaderTimeout = 5 * time.Second
	writeTimeout      = 30 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Status is the daemon's operational state, served at /v1/status.
//
// It answers the question a report cannot: which detector produced it. An
// inferred victim from the poller and a traced one from eBPF are the same shape,
// so anything asserting on behaviour, the e2e suite included, needs to be told
// rather than left to guess.
type Status struct {
	// Detector is the active detection method: ebpf, poller, or fake.
	Detector string `json:"detector"`
	// CgroupVersion is the hierarchy layout in use.
	CgroupVersion string `json:"cgroupVersion"`
	// Ready mirrors /readyz.
	Ready bool `json:"ready"`
	// Reports counts post-mortems produced since start.
	Reports uint64 `json:"reports"`
	// Skipped counts kills discarded as belonging to no Kubernetes container.
	Skipped uint64 `json:"skipped"`
	// Unattributed counts the subset of Skipped that came from inside the
	// kubepods tree. Skipped is expected to climb on any busy node; this
	// climbing means Kubernetes OOM kills went unreported, so it is the one to
	// alert on.
	Unattributed uint64 `json:"unattributed"`
	// TrackedCgroups is how many containers currently have sampled history.
	TrackedCgroups int `json:"trackedCgroups"`
	// UptimeSeconds is how long the daemon has been running.
	UptimeSeconds float64 `json:"uptimeSeconds"`
	// Version is the build the daemon was compiled from.
	Version string `json:"version"`
	// Node is the node whose pods are being watched. Empty when cluster
	// correlation is off or unavailable.
	Node string `json:"node,omitempty"`
	// PodCacheSynced reports whether the pod informer finished its initial
	// list. Reports carry namespace and pod names only once it has; before
	// that they identify pods by UID alone.
	PodCacheSynced bool `json:"podCacheSynced"`
	// PodsTracked is how many pods on this node the cache holds.
	PodsTracked int `json:"podsTracked"`
}

// Options configures a Server.
type Options struct {
	// Addr is the listen address, such as ":9090".
	Addr string
	// Store supplies reports. Required.
	Store store.Store
	// Ready reports whether the daemon is serving. Optional; nil means always.
	Ready func() bool
	// Status supplies the operational snapshot. Optional; nil serves zeroes.
	Status func() Status
	// Logger receives request errors.
	Logger *slog.Logger
}

// Server exposes the daemon's reports over HTTP.
type Server struct {
	store  store.Store
	status func() Status
	ready  func() bool
	log    *slog.Logger
	http   *http.Server
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("api requires a store")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Ready == nil {
		opts.Ready = func() bool { return true }
	}
	if opts.Status == nil {
		opts.Status = func() Status { return Status{Ready: opts.Ready()} }
	}

	s := &Server{store: opts.Store, status: opts.Status, ready: opts.Ready, log: opts.Logger}
	s.http = &http.Server{
		Addr:              opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
	}

	return s, nil
}

// Handler builds the route table. Exported so tests can exercise the routes
// with httptest rather than binding a port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/events", s.handleListEvents)
	mux.HandleFunc("GET /v1/events/{id}", s.handleGetEvent)
	return mux
}

// ListenAndServe runs the server until the context is cancelled, then shuts
// down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	errs := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("serving http: %w", err)
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}
		return nil
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeText(w, http.StatusOK, "ok\n")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready() {
		s.writeText(w, http.StatusServiceUnavailable, "not ready\n")
		return
	}
	s.writeText(w, http.StatusOK, "ready\n")
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	payload, err := json.Marshal(s.status())
	if err != nil {
		s.log.Error("rendering status", "error", err)
		s.writeError(w, http.StatusInternalServerError, "rendering status")
		return
	}
	s.writeJSON(w, http.StatusOK, append(payload, '\n'))
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := store.Filter{
		Namespace: query.Get("namespace"),
		Pod:       query.Get("pod"),
		Container: query.Get("container"),
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", raw))
			return
		}
		filter.Limit = limit
	}

	reports, err := s.store.List(r.Context(), filter)
	if err != nil {
		s.log.Error("listing reports", "error", err)
		s.writeError(w, http.StatusInternalServerError, "listing reports")
		return
	}

	payload, err := render.JSONList(reports)
	if err != nil {
		s.log.Error("rendering reports", "error", err)
		s.writeError(w, http.StatusInternalServerError, "rendering reports")
		return
	}
	s.writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusNotFound, "report not found")
		return
	}
	if err != nil {
		s.log.Error("getting report", "error", err)
		s.writeError(w, http.StatusInternalServerError, "getting report")
		return
	}

	payload, err := render.JSON(&report)
	if err != nil {
		s.log.Error("rendering report", "error", err)
		s.writeError(w, http.StatusInternalServerError, "rendering report")
		return
	}
	s.writeJSON(w, http.StatusOK, payload)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	// Reports carry values read from the node's filesystem (cgroup paths,
	// process command lines). Declaring the type and forbidding sniffing stops
	// a browser ever interpreting that as markup.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// gosec flags this as a taint sink. The payload is JSON this package
	// marshalled itself, served with an explicit type and nosniff, so it can
	// never be interpreted as markup.
	if _, err := w.Write(payload); err != nil { //nolint:gosec // G705: JSON we produced, typed and nosniff
		s.log.Debug("writing response", "error", err)
	}
}

func (s *Server) writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(body)); err != nil {
		s.log.Debug("writing response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	payload, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		s.writeText(w, http.StatusInternalServerError, "internal error\n")
		return
	}
	s.writeJSON(w, status, append(payload, '\n'))
}
