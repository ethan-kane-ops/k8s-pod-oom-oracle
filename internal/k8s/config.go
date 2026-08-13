// Package k8s resolves Kubernetes pod identities for the daemon.
//
// A cgroup path proves a pod UID and nothing more. Turning that into a
// namespace, pod, and container name needs the API server, which is what this
// package supplies: a node-scoped pod informer behind correlate.PodLookup.
package k8s

import (
	"errors"
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NodeNameEnv is the downward API variable the DaemonSet sets.
const NodeNameEnv = "NODE_NAME"

// Client-side rate limits. A node agent issues one list and then one long-lived
// watch per resource, so the client-go defaults are far more headroom than this
// needs, and headroom only turns a request storm from a bug into latency rather
// than an error.
const (
	clientQPS   = 5
	clientBurst = 10
)

// userAgent identifies the daemon in API server audit logs.
const userAgent = "oom-oracle"

// RestConfig builds API server credentials.
//
// In-cluster service account credentials are preferred, since running as a
// DaemonSet is the only way this daemon does real work. The kubeconfig fallback
// is for development: pointing a laptop at a remote cluster correlates nothing
// on that laptop, but it does prove the wiring without a deploy.
func RestConfig(kubeconfig string) (*rest.Config, error) {
	cfg, err := loadRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	cfg.QPS = clientQPS
	cfg.Burst = clientBurst
	cfg.UserAgent = userAgent

	return cfg, nil
}

func loadRestConfig(kubeconfig string) (*rest.Config, error) {
	// An explicit kubeconfig is an instruction, so it wins over the ambient
	// service account.
	if kubeconfig == "" {
		cfg, err := rest.InClusterConfig()
		switch {
		case err == nil:
			return cfg, nil
		case !errors.Is(err, rest.ErrNotInCluster):
			// Being in a cluster but unable to read the token is a real fault.
			// Falling through to look for a kubeconfig would report it as the
			// far more confusing "no configuration has been provided".
			return nil, fmt.Errorf("reading in-cluster config: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	return cfg, nil
}

// NewClient builds a clientset. It performs no I/O: a bad address surfaces on
// the first request, not here.
func NewClient(cfg *rest.Config) (kubernetes.Interface, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return client, nil
}

// NodeName resolves which node the daemon is watching.
//
// There is deliberately no cluster-wide fallback. A DaemonSet that failed to
// learn its node name would otherwise start watching every pod in the cluster,
// which on a large one is a quiet way to turn a diagnostic tool into an
// incident.
func NodeName(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if name := os.Getenv(NodeNameEnv); name != "" {
		return name, nil
	}
	return "", fmt.Errorf(
		"node name is unknown: pass --node-name or set %s from the downward API", NodeNameEnv)
}
