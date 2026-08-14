package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// fakeDaemon serves reports over the same route the real daemon uses, and
// records the query it was asked, so filter wiring can be asserted without a
// cluster.
type fakeDaemon struct {
	server  *httptest.Server
	reports []oom.Report
	status  int
	body    string

	lastQuery url.Values
}

func newFakeDaemon(t *testing.T, reports ...oom.Report) *fakeDaemon {
	t.Helper()

	d := &fakeDaemon{reports: reports, status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		d.lastQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.status)
		if d.body != "" {
			_, _ = w.Write([]byte(d.body))
			return
		}
		_ = json.NewEncoder(w).Encode(d.reports)
	})

	d.server = httptest.NewServer(mux)
	t.Cleanup(d.server.Close)
	return d
}

func (d *fakeDaemon) addr() string { return d.server.URL }

func report(id, namespace, pod, container string) oom.Report {
	return oom.Report{
		ID: id,
		Identity: correlate.Identity{
			Namespace: namespace, PodName: pod, ContainerName: container, Resolved: true,
		},
	}
}

func TestRunInspectRejectsUnknownFormat(t *testing.T) {
	t.Parallel()

	err := runInspect(context.Background(), &bytes.Buffer{}, inspectConfig{format: "yaml"}, "")
	if err == nil {
		t.Fatal("runInspect() with an unknown format = nil error, want an error")
	}
	// Checked before the request, so an unreachable daemon cannot mask a typo.
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error %q does not name the rejected format", err)
	}
}

func TestRunInspectRendersText(t *testing.T) {
	t.Parallel()

	daemon := newFakeDaemon(t, report("a", "default", "api", "web"))

	var out bytes.Buffer
	cfg := inspectConfig{daemonAddr: daemon.addr(), format: formatText}
	if err := runInspect(context.Background(), &out, cfg, ""); err != nil {
		t.Fatalf("runInspect() error = %v", err)
	}

	if !strings.Contains(out.String(), "api") {
		t.Errorf("output does not name the pod:\n%s", out.String())
	}
}

func TestRunInspectRendersJSON(t *testing.T) {
	t.Parallel()

	daemon := newFakeDaemon(t, report("a", "default", "api", "web"), report("b", "default", "api", "sidecar"))

	var out bytes.Buffer
	cfg := inspectConfig{daemonAddr: daemon.addr(), format: formatJSON}
	if err := runInspect(context.Background(), &out, cfg, ""); err != nil {
		t.Fatalf("runInspect() error = %v", err)
	}

	var got []oom.Report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decoding output: %v (output %s)", err, out.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2", len(got))
	}
}

// An empty result is a normal answer, not an error. A cluster with no OOM kills
// is the state everyone wants to be in.
func TestRunInspectWithNoReports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		format string
		want   string
	}{
		{name: "text says so plainly", format: formatText, want: "no OOM kills recorded"},
		{name: "json stays a valid empty list", format: formatJSON, want: "[]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			daemon := newFakeDaemon(t)

			var out bytes.Buffer
			cfg := inspectConfig{daemonAddr: daemon.addr(), format: tt.format}
			if err := runInspect(context.Background(), &out, cfg, ""); err != nil {
				t.Fatalf("runInspect() error = %v", err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), tt.want)
			}
		})
	}
}

// Reports run to dozens of lines each, so consecutive ones need a visible
// break or a reader cannot tell where one post-mortem ends.
func TestRunInspectSeparatesMultipleReports(t *testing.T) {
	t.Parallel()

	daemon := newFakeDaemon(t,
		report("a", "default", "api", "web"),
		report("b", "default", "worker", "app"),
	)

	var out bytes.Buffer
	cfg := inspectConfig{daemonAddr: daemon.addr(), format: formatText}
	if err := runInspect(context.Background(), &out, cfg, ""); err != nil {
		t.Fatalf("runInspect() error = %v", err)
	}

	if !strings.Contains(out.String(), "────") {
		t.Errorf("consecutive reports are not separated:\n%s", out.String())
	}
}

func TestFetchReportsSendsFilters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  inspectConfig
		pod  string
		want map[string]string
		bare []string
	}{
		{
			name: "no filters sends no query",
			bare: []string{"pod", "namespace", "container", "limit"},
		},
		{
			name: "pod",
			pod:  "api-7d9",
			want: map[string]string{"pod": "api-7d9"},
		},
		{
			name: "namespace and container",
			cfg:  inspectConfig{namespace: "default", container: "web"},
			want: map[string]string{"namespace": "default", "container": "web"},
		},
		{
			name: "limit",
			cfg:  inspectConfig{limit: 5},
			want: map[string]string{"limit": "5"},
		},
		{
			// Zero means "no limit", so sending limit=0 would ask the daemon
			// for nothing at all.
			name: "a zero limit is omitted",
			cfg:  inspectConfig{limit: 0},
			bare: []string{"limit"},
		},
		{
			name: "everything at once",
			cfg:  inspectConfig{namespace: "default", container: "web", limit: 3},
			pod:  "api-7d9",
			want: map[string]string{
				"namespace": "default", "container": "web", "limit": "3", "pod": "api-7d9",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			daemon := newFakeDaemon(t)
			cfg := tt.cfg
			cfg.daemonAddr = daemon.addr()

			if _, err := fetchReports(context.Background(), cfg, tt.pod); err != nil {
				t.Fatalf("fetchReports() error = %v", err)
			}

			for key, want := range tt.want {
				if got := daemon.lastQuery.Get(key); got != want {
					t.Errorf("query %s = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.bare {
				if daemon.lastQuery.Has(key) {
					t.Errorf("query carries %s = %q, want it absent",
						key, daemon.lastQuery.Get(key))
				}
			}
		})
	}
}

func TestFetchReportsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(*fakeDaemon) string
		wantMsg string
	}{
		{
			name:    "an unparseable address",
			setup:   func(*fakeDaemon) string { return "://not-a-url" },
			wantMsg: "parsing daemon address",
		},
		{
			name:    "an unreachable daemon",
			setup:   func(d *fakeDaemon) string { d.server.Close(); return d.addr() },
			wantMsg: "querying daemon",
		},
		{
			name: "a non-200 response",
			setup: func(d *fakeDaemon) string {
				d.status = http.StatusServiceUnavailable
				return d.addr()
			},
			wantMsg: "daemon returned",
		},
		{
			name:    "a body that is not JSON",
			setup:   func(d *fakeDaemon) string { d.body = "not json"; return d.addr() },
			wantMsg: "decoding daemon response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			daemon := newFakeDaemon(t)
			addr := tt.setup(daemon)

			_, err := fetchReports(context.Background(), inspectConfig{daemonAddr: addr}, "")
			if err == nil {
				t.Fatal("fetchReports() = nil error, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// A trailing slash on --daemon is the kind of thing a shell completion adds.
// It must not produce a double slash the mux fails to route.
func TestFetchReportsTrimsATrailingSlash(t *testing.T) {
	t.Parallel()

	daemon := newFakeDaemon(t, report("a", "default", "api", "web"))

	got, err := fetchReports(context.Background(), inspectConfig{daemonAddr: daemon.addr() + "/"}, "")
	if err != nil {
		t.Fatalf("fetchReports() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d reports, want 1", len(got))
	}
}

func TestRunInspectPropagatesFetchErrors(t *testing.T) {
	t.Parallel()

	cfg := inspectConfig{daemonAddr: "://not-a-url", format: formatText}
	if err := runInspect(context.Background(), &bytes.Buffer{}, cfg, ""); err == nil {
		t.Fatal("runInspect() with an unreachable daemon = nil error, want an error")
	}
}

func TestSetIfNotEmpty(t *testing.T) {
	t.Parallel()

	query := url.Values{}
	setIfNotEmpty(query, "set", "value")
	setIfNotEmpty(query, "unset", "")

	if got := query.Get("set"); got != "value" {
		t.Errorf("set = %q, want %q", got, "value")
	}
	if query.Has("unset") {
		t.Error("an empty value was written to the query")
	}
}

// errWriter fails every write, standing in for a closed pipe. `inspect | head`
// does exactly this once head has seen enough.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestWriteAllWrapsWriteErrors(t *testing.T) {
	t.Parallel()

	err := writeAll(errWriter{}, []byte("payload"))
	if err == nil {
		t.Fatal("writeAll() to a failing writer = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "writing output") {
		t.Errorf("error = %q, want it to mention writing output", err)
	}
}

func TestRunInspectReportsOutputFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		format  string
		reports []oom.Report
	}{
		{name: "json", format: formatJSON, reports: []oom.Report{report("a", "default", "api", "web")}},
		{name: "text", format: formatText, reports: []oom.Report{report("a", "default", "api", "web")}},
		{name: "text with no reports", format: formatText},
		{
			name:   "text between reports",
			format: formatText,
			reports: []oom.Report{
				report("a", "default", "api", "web"),
				report("b", "default", "worker", "app"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			daemon := newFakeDaemon(t, tt.reports...)
			cfg := inspectConfig{daemonAddr: daemon.addr(), format: tt.format}

			if err := runInspect(context.Background(), errWriter{}, cfg, ""); err == nil {
				t.Fatal("runInspect() to a failing writer = nil error, want an error")
			}
		})
	}
}

// The pod/<name> form is what kubectl prints, so it has to be accepted and
// stripped rather than sent to the daemon as a literal pod name.
func TestInspectCmdStripsThePodPrefix(t *testing.T) {
	t.Parallel()

	daemon := newFakeDaemon(t)

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"inspect", "pod/api-7d9", "--daemon", daemon.addr()})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := daemon.lastQuery.Get("pod"); got != "api-7d9" {
		t.Errorf("pod query = %q, want %q", got, "api-7d9")
	}
}

func TestInspectCmdFlagDefaults(t *testing.T) {
	t.Parallel()

	cmd := newInspectCmd()

	tests := []struct {
		flag string
		want string
	}{
		{flag: "daemon", want: "http://127.0.0.1:9090"},
		{flag: "output", want: formatText},
		{flag: "namespace", want: ""},
		{flag: "container", want: ""},
	}

	for _, tt := range tests {
		flag := cmd.Flags().Lookup(tt.flag)
		if flag == nil {
			t.Errorf("--%s is not registered", tt.flag)
			continue
		}
		if flag.DefValue != tt.want {
			t.Errorf("--%s default = %q, want %q", tt.flag, flag.DefValue, tt.want)
		}
	}
}
