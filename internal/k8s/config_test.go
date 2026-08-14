package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kubeconfig is the smallest document clientcmd will accept.
const kubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://api.test.example:6443
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user:
      token: not-a-real-token
`

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func TestRestConfigFromKubeconfig(t *testing.T) {
	got, err := RestConfig(writeKubeconfig(t, kubeconfig))
	if err != nil {
		t.Fatalf("RestConfig() error = %v", err)
	}

	if got.Host != "https://api.test.example:6443" {
		t.Errorf("Host = %q, want the server from the kubeconfig", got.Host)
	}
	// The limits exist so a bug shows up as errors rather than as a quiet flood
	// of requests at the API server, so they are worth asserting.
	if got.QPS != clientQPS || got.Burst != clientBurst {
		t.Errorf("QPS/Burst = %v/%d, want %v/%d", got.QPS, got.Burst, clientQPS, clientBurst)
	}
	if got.UserAgent != userAgent {
		t.Errorf("UserAgent = %q, want %q", got.UserAgent, userAgent)
	}
}

// TestRestConfigPrefersAnExplicitKubeconfig covers a developer pointing the
// daemon at a remote cluster from a machine that also looks like it is inside
// one.
func TestRestConfigPrefersAnExplicitKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	got, err := RestConfig(writeKubeconfig(t, kubeconfig))
	if err != nil {
		t.Fatalf("RestConfig() error = %v", err)
	}
	if got.Host != "https://api.test.example:6443" {
		t.Errorf("Host = %q, want the explicit kubeconfig to win over in-cluster", got.Host)
	}
}

// TestRestConfigReportsBrokenInClusterCredentials covers the case the fallback
// must not swallow: the daemon is in a cluster, but its service account token
// cannot be read. Falling through to kubeconfig loading would report that as
// "no configuration has been provided", which sends the reader somewhere else
// entirely.
func TestRestConfigReportsBrokenInClusterCredentials(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	_, err := RestConfig("")
	if err == nil {
		t.Fatal("RestConfig() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "in-cluster") {
		t.Errorf("error = %v, want it to name the in-cluster config as the cause", err)
	}
}

func TestRestConfigRejectsMissingKubeconfig(t *testing.T) {
	if _, err := RestConfig(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("RestConfig() error = nil for a missing kubeconfig, want an error")
	}
}

func TestNewClientFromConfig(t *testing.T) {
	cfg, err := RestConfig(writeKubeconfig(t, kubeconfig))
	if err != nil {
		t.Fatalf("RestConfig() error = %v", err)
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClient() returned a nil client")
	}
}
