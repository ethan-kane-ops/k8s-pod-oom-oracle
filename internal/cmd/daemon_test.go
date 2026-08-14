package cmd

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/api"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/daemon"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/k8s"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// cgroupFixture is a minimal unified hierarchy with a memory controller.
func cgroupFixture(t *testing.T) *cgroup.FS {
	t.Helper()

	fsys := fstest.MapFS{
		"cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
		"memory.current":     &fstest.MapFile{Data: []byte("0\n")},
		"memory.max":         &fstest.MapFile{Data: []byte("max\n")},
	}
	cg, err := cgroup.New(fsys)
	if err != nil {
		t.Fatalf("cgroup.New() error = %v", err)
	}
	return cg
}

func procFixture() *procfs.FS { return procfs.New(fstest.MapFS{}) }

// writeKubeconfig writes a syntactically valid kubeconfig. No server is ever
// contacted: client construction performs no I/O, which is exactly what lets
// the wiring be tested without a cluster.
func writeKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	const contents = `apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user:
      token: test-token
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func TestBuildDetector(t *testing.T) {
	tests := []struct {
		name       string
		detector   string
		wantErr    bool
		wantSource detector.Source
	}{
		{name: "poller", detector: detectorPoller, wantSource: detector.SourcePoller},
		{name: "unknown", detector: "tracepoint", wantErr: true},
		{name: "empty", detector: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := daemonConfig{detectorName: tt.detector, pollEvery: 10 * time.Millisecond}

			got, err := buildDetector(cfg, cgroupFixture(t), procFixture(), discardLogger())
			if tt.wantErr {
				if err == nil {
					_ = got.Close()
					t.Fatalf("buildDetector(%q) = nil error, want an error", tt.detector)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildDetector(%q) error = %v", tt.detector, err)
			}
			t.Cleanup(func() { _ = got.Close() })

			if got.Source() != tt.wantSource {
				t.Errorf("source = %q, want %q", got.Source(), tt.wantSource)
			}
		})
	}
}

// auto must always produce a working detector. Which one it picks depends on
// the kernel and privileges of the machine running the test, so the assertion
// is that something usable comes back, not which.
func TestBuildDetectorAutoAlwaysSucceeds(t *testing.T) {
	cfg := daemonConfig{detectorName: detectorAuto, pollEvery: 10 * time.Millisecond}

	got, err := buildDetector(cfg, cgroupFixture(t), procFixture(), discardLogger())
	if err != nil {
		t.Fatalf("buildDetector(auto) error = %v; auto must fall back rather than fail", err)
	}
	t.Cleanup(func() { _ = got.Close() })

	switch got.Source() {
	case detector.SourceEBPF, detector.SourcePoller:
	default:
		t.Errorf("source = %q, want ebpf or poller", got.Source())
	}
}

// An explicit --detector=ebpf must never silently downgrade. Someone who asked
// for traced victims should be told the kernel refused, not handed inferred
// ones that look identical in every field but a boolean.
func TestBuildDetectorEBPFNeverFallsBack(t *testing.T) {
	cfg := daemonConfig{detectorName: detectorEBPF, pollEvery: 10 * time.Millisecond}

	got, err := buildDetector(cfg, cgroupFixture(t), procFixture(), discardLogger())
	if err != nil {
		return // The kernel or the privileges are absent, which is the point.
	}
	t.Cleanup(func() { _ = got.Close() })

	if got.Source() != detector.SourceEBPF {
		t.Errorf("source = %q, want ebpf: an explicit request fell back", got.Source())
	}
}

func TestBuildPodCache(t *testing.T) {
	tests := []struct {
		name       string
		kubernetes string
		nodeName   string
		kubeconfig func(*testing.T) string
		wantErr    bool
		wantCache  bool
	}{
		{
			name:       "off never contacts the api server",
			kubernetes: kubernetesOff,
		},
		{
			// off must win even with everything else configured, or the flag
			// does not mean what it says.
			name:       "off wins over a usable config",
			kubernetes: kubernetesOff,
			nodeName:   "worker-1",
			kubeconfig: writeKubeconfig,
		},
		{
			name:       "unknown value is rejected",
			kubernetes: "yes",
			wantErr:    true,
		},
		{
			name:       "auto degrades when the node name is unknown",
			kubernetes: kubernetesAuto,
			kubeconfig: writeKubeconfig,
		},
		{
			name:       "auto degrades when credentials are unusable",
			kubernetes: kubernetesAuto,
			nodeName:   "worker-1",
			kubeconfig: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		},
		{
			name:       "on fails when the node name is unknown",
			kubernetes: kubernetesOn,
			kubeconfig: writeKubeconfig,
			wantErr:    true,
		},
		{
			name:       "on fails when credentials are unusable",
			kubernetes: kubernetesOn,
			nodeName:   "worker-1",
			kubeconfig: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
			wantErr:    true,
		},
		{
			name:       "auto builds a cache when everything is present",
			kubernetes: kubernetesAuto,
			nodeName:   "worker-1",
			kubeconfig: writeKubeconfig,
			wantCache:  true,
		},
		{
			name:       "on builds a cache when everything is present",
			kubernetes: kubernetesOn,
			nodeName:   "worker-1",
			kubeconfig: writeKubeconfig,
			wantCache:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// NodeName falls back to the environment, so a NODE_NAME set on the
			// machine running the tests would otherwise decide the outcome.
			t.Setenv(k8s.NodeNameEnv, "")

			cfg := daemonConfig{kubernetes: tt.kubernetes, nodeName: tt.nodeName}
			if tt.kubeconfig != nil {
				cfg.kubeconfig = tt.kubeconfig(t)
			}

			got, err := buildPodCache(cfg, discardLogger())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildPodCache() = %v, nil error, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildPodCache() error = %v", err)
			}
			if (got != nil) != tt.wantCache {
				t.Fatalf("cache present = %t, want %t", got != nil, tt.wantCache)
			}
			if tt.wantCache && got.Node() != tt.nodeName {
				t.Errorf("node = %q, want %q", got.Node(), tt.nodeName)
			}
		})
	}
}

// The downward API is the supported way to supply the node name, so the
// environment must work with no flag at all.
func TestBuildPodCacheReadsTheDownwardAPI(t *testing.T) {
	t.Setenv(k8s.NodeNameEnv, "worker-from-env")

	cfg := daemonConfig{kubernetes: kubernetesOn, kubeconfig: writeKubeconfig(t)}

	got, err := buildPodCache(cfg, discardLogger())
	if err != nil {
		t.Fatalf("buildPodCache() error = %v", err)
	}
	if got == nil {
		t.Fatal("buildPodCache() = nil cache, want one built from NODE_NAME")
	}
	if got.Node() != "worker-from-env" {
		t.Errorf("node = %q, want %q", got.Node(), "worker-from-env")
	}
}

// The flag is an explicit instruction and outranks the ambient environment.
func TestBuildPodCacheFlagBeatsTheEnvironment(t *testing.T) {
	t.Setenv(k8s.NodeNameEnv, "worker-from-env")

	cfg := daemonConfig{
		kubernetes: kubernetesOn,
		nodeName:   "worker-from-flag",
		kubeconfig: writeKubeconfig(t),
	}

	got, err := buildPodCache(cfg, discardLogger())
	if err != nil {
		t.Fatalf("buildPodCache() error = %v", err)
	}
	if got.Node() != "worker-from-flag" {
		t.Errorf("node = %q, want %q", got.Node(), "worker-from-flag")
	}
}

func TestNewPodCacheRequiresANodeName(t *testing.T) {
	t.Setenv(k8s.NodeNameEnv, "")

	_, err := newPodCache(daemonConfig{kubeconfig: writeKubeconfig(t)}, discardLogger())
	if err == nil {
		t.Fatal("newPodCache() without a node name = nil error, want an error")
	}
	if !strings.Contains(err.Error(), k8s.NodeNameEnv) {
		t.Errorf("error = %q, want it to name %s", err, k8s.NodeNameEnv)
	}
}

func newTestPodCache(t *testing.T, pods ...*corev1.Pod) *k8s.PodCache {
	t.Helper()

	objects := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
	}

	client := fake.NewClientset(objects...)
	cache, err := k8s.New(k8s.Options{Client: client, Node: "worker-1", Logger: discardLogger()})
	if err != nil {
		t.Fatalf("k8s.New() error = %v", err)
	}
	return cache
}

func TestAwaitPodCacheLogsASyncedCache(t *testing.T) {
	cache := newTestPodCache(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: types.UID("uid-1")},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = cache.Run(ctx) }()

	// Returns once the informer has listed, which is all this function waits on.
	awaitPodCache(ctx, cache, discardLogger())

	if !cache.Synced() {
		t.Error("cache reports unsynced after awaitPodCache returned")
	}
}

// A daemon shutting down must not sit in the sync wait. The cache is a
// convenience; the detector is the job.
func TestAwaitPodCacheReturnsOnCancellation(t *testing.T) {
	cache := newTestPodCache(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		awaitPodCache(ctx, cache, discardLogger())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(k8s.DefaultSyncTimeout):
		t.Fatal("awaitPodCache did not return on a cancelled context")
	}
}

func newTestDaemon(t *testing.T) (*daemon.Daemon, *cgroup.FS) {
	t.Helper()

	cg := cgroupFixture(t)
	memorySampler, err := sampler.New(sampler.Options{FS: cg, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("sampler.New() error = %v", err)
	}

	oracle, err := daemon.New(daemon.Options{
		Detector: detector.NewFake(),
		Sampler:  memorySampler,
		Store:    store.NewMemory(1),
		Cgroup:   cg,
		Resolver: correlate.NewResolver(nil),
		Proc:     procFixture(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("daemon.New() error = %v", err)
	}
	return oracle, cg
}

func TestDaemonStatus(t *testing.T) {
	oracle, cg := newTestDaemon(t)
	started := time.Now().Add(-90 * time.Second)

	got := daemonStatus(oracle, cg, nil, started)

	if got.Detector != string(detector.SourceFake) {
		t.Errorf("detector = %q, want %q", got.Detector, detector.SourceFake)
	}
	if got.CgroupVersion != cg.Version().String() {
		t.Errorf("cgroupVersion = %q, want %q", got.CgroupVersion, cg.Version())
	}
	if got.UptimeSeconds < 90 {
		t.Errorf("uptimeSeconds = %v, want at least 90", got.UptimeSeconds)
	}
	if got.Version == "" {
		t.Error("version is empty; /v1/status is how a deployed build identifies itself")
	}
	// With correlation off these must stay zero rather than assert a node the
	// daemon never learned.
	if got.Node != "" || got.PodCacheSynced || got.PodsTracked != 0 {
		t.Errorf("cluster fields are populated without a cache: %+v", got)
	}
}

func TestDaemonStatusIncludesTheCache(t *testing.T) {
	oracle, cg := newTestDaemon(t)
	cache := newTestPodCache(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: types.UID("uid-1")},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cache.Run(ctx) }()
	awaitPodCache(ctx, cache, discardLogger())

	got := daemonStatus(oracle, cg, cache, time.Now())

	if got.Node != "worker-1" {
		t.Errorf("node = %q, want %q", got.Node, "worker-1")
	}
	if !got.PodCacheSynced {
		t.Error("podCacheSynced = false after a successful sync")
	}
	if got.PodsTracked != 1 {
		t.Errorf("podsTracked = %d, want 1", got.PodsTracked)
	}
}

// The status snapshot is what the e2e suite asserts on, so it has to be
// marshallable as api.Status without loss.
func TestDaemonStatusIsAnAPIStatus(t *testing.T) {
	oracle, cg := newTestDaemon(t)

	var got any = daemonStatus(oracle, cg, nil, time.Now())
	if _, ok := got.(api.Status); !ok {
		t.Fatalf("daemonStatus returned %T, want api.Status", got)
	}
}

func TestDaemonCmdFlagDefaults(t *testing.T) {
	t.Parallel()

	cmd := newDaemonCmd()

	tests := []struct {
		flag string
		want string
	}{
		{flag: "detector", want: detectorAuto},
		{flag: "kubernetes", want: kubernetesAuto},
		{flag: "cgroup-root", want: "/sys/fs/cgroup"},
		{flag: "proc-root", want: "/proc"},
		{flag: "cgroup-prefix", want: "/"},
		{flag: "listen", want: ":9090"},
		{flag: "log-level", want: "info"},
		{flag: "log-format", want: "text"},
		{flag: "include-non-kubernetes", want: "false"},
		{flag: "node-name", want: ""},
		{flag: "kubeconfig", want: ""},
		{flag: "sample-interval", want: sampler.DefaultInterval.String()},
		{flag: "poll-interval", want: detector.DefaultPollInterval.String()},
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

// cgroupDir writes a unified hierarchy to disk. runDaemon opens its roots with
// os.DirFS, so an in-memory fixture cannot reach it.
func cgroupDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	files := map[string]string{
		"cgroup.controllers": "memory\n",
		"memory.current":     "0\n",
		"memory.max":         "max\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return dir
}

// The assembled daemon must start, serve, and shut down cleanly on
// cancellation. A kubelet eviction is a SIGTERM, and an unclean exit there
// means a pod stuck Terminating.
func TestRunDaemonStartsAndStopsCleanly(t *testing.T) {
	cfg := daemonConfig{
		cgroupRoot:   cgroupDir(t),
		procRoot:     t.TempDir(),
		cgroupPrefix: "/",
		detectorName: detectorPoller,
		listenAddr:   "127.0.0.1:0",
		sampleEvery:  10 * time.Millisecond,
		pollEvery:    10 * time.Millisecond,
		historySize:  8,
		retain:       8,
		kubernetes:   kubernetesOff,
		logLevel:     "error",
		logFormat:    "text",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, &cobra.Command{}, cfg) }()

	// Let the pipeline actually come up before tearing it down, so this
	// exercises a running daemon rather than a cancelled construction.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDaemon() error = %v, want nil on cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}
}

func TestRunDaemonRejectsBadConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*daemonConfig)
	}{
		{
			name:   "unknown detector",
			mutate: func(c *daemonConfig) { c.detectorName = "tracepoint" },
		},
		{
			name:   "unknown kubernetes mode",
			mutate: func(c *daemonConfig) { c.kubernetes = "yes" },
		},
		{
			name:   "unknown log level",
			mutate: func(c *daemonConfig) { c.logLevel = "trace" },
		},
		{
			name:   "a cgroup root with no memory controller",
			mutate: func(c *daemonConfig) { c.cgroupRoot = t.TempDir() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := daemonConfig{
				cgroupRoot:   cgroupDir(t),
				procRoot:     t.TempDir(),
				cgroupPrefix: "/",
				detectorName: detectorPoller,
				listenAddr:   "127.0.0.1:0",
				sampleEvery:  10 * time.Millisecond,
				pollEvery:    10 * time.Millisecond,
				historySize:  8,
				retain:       8,
				kubernetes:   kubernetesOff,
				logLevel:     "error",
				logFormat:    "text",
			}
			tt.mutate(&cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := runDaemon(ctx, &cobra.Command{}, cfg); err == nil {
				t.Fatal("runDaemon() = nil error, want an error")
			}
		})
	}
}

func TestRunDaemonRejectsBadFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "unknown log level", args: []string{"daemon", "--log-level=trace"}},
		{name: "unknown log format", args: []string{"daemon", "--log-format=logfmt"}},
		{name: "unreadable cgroup root", args: []string{"daemon", "--cgroup-root=/nonexistent"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := NewRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(tt.args)

			if err := root.Execute(); err == nil {
				t.Fatalf("Execute(%v) = nil error, want an error", tt.args)
			}
		})
	}
}
