//go:build e2e

// Package e2e drives a real cluster.
//
// Nothing here is mocked. A pod really exceeds its memory limit, the kernel
// really kills a process, and the assertions are on what the deployed daemon
// reported about it. That is deliberate: every bug this project has hit in
// anger was invisible to fixture-based tests and only appeared against a live
// kernel.
//
// Build-tagged so `go test ./...` stays a unit run. Use `just e2e`.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	// daemonNamespace is where deploy/ installs the agent.
	daemonNamespace = "oom-oracle"
	// workloadNamespace holds the pods each test kills.
	workloadNamespace = "oom-oracle-e2e"
	// apiPort must match the containerPort in deploy/daemonset.yaml.
	apiPort = 9090
	// workloadImage is what every test pod runs. It must match
	// e2e_workload_image in the justfile, which builds it and loads it into the
	// kind node before the suite starts.
	//
	// The tag exists in no registry, and the manifests pin
	// imagePullPolicy: Never against it. That is the point: if the preload ever
	// stops matching this constant, the pod fails immediately with
	// ErrImageNeverPull naming the image, rather than sitting in Pending while
	// a report timeout expires and blames the daemon.
	workloadImage = "oom-oracle-workload:e2e"
)

// kubectl runs a kubectl command and returns its stdout.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()

	out, err := runKubectl(args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func runKubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// daemonPod returns the name of the agent pod on the node.
func daemonPod(t *testing.T) string {
	t.Helper()

	out := kubectl(t, "get", "pods", "-n", daemonNamespace,
		"-l", "app.kubernetes.io/name=oom-oracle",
		"-o", "jsonpath={.items[0].metadata.name}")
	if out == "" {
		t.Fatal("no oom-oracle pod found; is the DaemonSet deployed?")
	}
	return out
}

// daemonAPI performs a GET against the deployed daemon's HTTP API.
//
// It goes through the API server's pod proxy rather than kubectl port-forward,
// which would mean managing a background process and a race on the local port
// for every call.
func daemonAPI(t *testing.T, pod, path string) []byte {
	t.Helper()

	raw := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy%s",
		daemonNamespace, pod, apiPort, path)
	return []byte(kubectl(t, "get", "--raw", raw))
}

// status is the shape of /v1/status.
type status struct {
	Detector       string  `json:"detector"`
	CgroupVersion  string  `json:"cgroupVersion"`
	Ready          bool    `json:"ready"`
	Reports        uint64  `json:"reports"`
	Skipped        uint64  `json:"skipped"`
	TrackedCgroups int     `json:"trackedCgroups"`
	UptimeSeconds  float64 `json:"uptimeSeconds"`
	Version        string  `json:"version"`
	Node           string  `json:"node"`
	PodCacheSynced bool    `json:"podCacheSynced"`
	PodsTracked    int     `json:"podsTracked"`
}

// report mirrors the fields the suite asserts on. It is deliberately partial:
// this is a consumer of the JSON contract, and decoding through the producer's
// own structs would hide a breaking rename from both sides.
type report struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Identity struct {
		PodUID        string `json:"podUID"`
		ContainerID   string `json:"containerID"`
		QoS           string `json:"qos"`
		CgroupPath    string `json:"cgroupPath"`
		Resolved      bool   `json:"resolved"`
		Namespace     string `json:"namespace"`
		PodName       string `json:"podName"`
		ContainerName string `json:"containerName"`
		Image         string `json:"image"`
	} `json:"identity"`
	Victim struct {
		PID      int      `json:"pid"`
		NSPid    int      `json:"nsPid"`
		Comm     string   `json:"comm"`
		Cmdline  []string `json:"cmdline"`
		RSSBytes uint64   `json:"rssBytes"`
		Inferred bool     `json:"inferred"`
		Known    bool     `json:"known"`
	} `json:"victim"`
	KillCount  uint64 `json:"killCount"`
	LimitBytes uint64 `json:"limitBytes"`
	PeakBytes  uint64 `json:"peakBytes"`
	Trajectory []struct {
		UsedBytes  uint64  `json:"usedBytes"`
		LimitBytes uint64  `json:"limitBytes"`
		Ratio      float64 `json:"ratio"`
	} `json:"trajectory"`
	Hogs []struct {
		PID  int    `json:"pid"`
		Comm string `json:"comm"`
	} `json:"hogs"`
}

func daemonStatus(t *testing.T, pod string) status {
	t.Helper()

	var got status
	if err := json.Unmarshal(daemonAPI(t, pod, "/v1/status"), &got); err != nil {
		t.Fatalf("decoding /v1/status: %v", err)
	}
	return got
}

func daemonReports(t *testing.T, pod string) []report {
	t.Helper()

	var got []report
	if err := json.Unmarshal(daemonAPI(t, pod, "/v1/events"), &got); err != nil {
		t.Fatalf("decoding /v1/events: %v", err)
	}
	return got
}

// eventually polls until check passes, failing with the last error.
//
// Every wait in this suite is a poll rather than a sleep. A kill is observed
// within a poll interval and reported some milliseconds later; a fixed sleep
// long enough to be safe on a loaded CI runner would make the suite
// unreasonably slow on a laptop.
func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()

	eventuallyDiag(t, timeout, what, check, nil)
}

// eventuallyDiag is eventually with a diagnosis attached to the timeout.
//
// diagnose runs once, after the deadline passes, never inside the loop. It
// costs several API calls, and only the final failure message is ever read, so
// gathering it on every poll would be a hundredfold waste that also slows the
// polling it is meant to explain.
func eventuallyDiag(t *testing.T, timeout time.Duration, what string, check func() error, diagnose func() string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		last = check()
		switch {
		case last == nil:
			return
		case errors.Is(last, errTerminal):
			t.Fatalf("waiting for %s: %v", what, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if diagnose != nil {
		t.Fatalf("timed out after %s waiting for %s: %v; %s", timeout, what, last, diagnose())
	}
	t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
}

// errTerminal marks a condition that polling cannot resolve, so eventuallyDiag
// stops rather than serving out its timeout. A missing image is the case that
// matters: the kubelet decides in under a second and never revisits it, and
// spending the remaining ninety seconds re-reading that answer only delays a
// message that was already final.
var errTerminal = errors.New("terminal")

// applyPod creates a pod from a manifest. It does not wait for anything: the
// API server accepts the object long before the pod is scheduled, let alone
// running. Callers that need a started container use waitForPodStarted.
func applyPod(t *testing.T, manifest string) {
	t.Helper()

	cmd := exec.Command("kubectl", "apply", "-n", workloadNamespace, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("applying pod: %v: %s", err, stderr.String())
	}
}

// waitForPodStarted blocks until the pod's container has begun executing.
//
// Without this, the report timeout silently covers scheduling and container
// startup as well as detection, so anything slow before the first instruction
// runs is reported as the daemon having missed a kill.
//
// The condition is "no longer waiting" rather than "running", because a
// container that exceeds its limit quickly can be Terminated before any poll
// observes it Running. Both mean the same thing here: it executed.
func waitForPodStarted(t *testing.T, name string) {
	t.Helper()

	// Asking for the startedAt timestamps rather than for the state object keeps
	// this off kubectl's rendering of a map. Either being present means the
	// container executed. The waiting reason comes back in the same call so a
	// poll costs one request rather than two.
	const state = "{.status.containerStatuses[0].state.running.startedAt}" +
		"{.status.containerStatuses[0].state.terminated.startedAt}" +
		"{\"\\t\"}{.status.containerStatuses[0].state.waiting.reason}"

	eventuallyDiag(t, 90*time.Second, "pod "+name+" to start its container", func() error {
		out, err := runKubectl("get", "pod", name, "-n", workloadNamespace, "-o", "jsonpath="+state)
		if err != nil {
			return err
		}
		startedAt, reason, _ := strings.Cut(out, "\t")
		if strings.TrimSpace(startedAt) != "" {
			return nil
		}
		if terminalWaitReasons[strings.TrimSpace(reason)] {
			return fmt.Errorf("%w: pod %s cannot start: %s; %s",
				errTerminal, name, strings.TrimSpace(reason), podDiagnostics(t, name))
		}
		return errors.New("container is still waiting")
	}, func() string { return podDiagnostics(t, name) })
}

// terminalWaitReasons are container waiting reasons that no amount of waiting
// resolves. Every one of them means the image the suite expects to be preloaded
// is not on the node, which is a setup fault, not a slow start.
var terminalWaitReasons = map[string]bool{
	"ErrImageNeverPull": true,
	"InvalidImageName":  true,
	"ImagePullBackOff":  true,
	"ErrImagePull":      true,
}

// podDiagnostics summarises why a pod is not doing what a test expected.
//
// A failure that says only "no report arrived" cannot distinguish a daemon that
// missed a kill from a pod that never ran, and those have nothing in common.
func podDiagnostics(t *testing.T, name string) string {
	t.Helper()

	const summary = "{.metadata.uid}{\"\\t\"}{.status.phase}{\"\\t\"}" +
		"{.status.containerStatuses[*].state}"

	out, err := runKubectl("get", "pod", name, "-n", workloadNamespace, "-o", "jsonpath="+summary)
	if err != nil {
		return fmt.Sprintf("pod %s could not be read: %v", name, err)
	}
	uid, rest, _ := strings.Cut(out, "\t")
	phase, state, _ := strings.Cut(rest, "\t")

	// Events are selected by UID, not by name. Every test reuses a fixed pod
	// name and events outlive the object, so selecting by name mixes in the
	// previous run's pod and prints "Container started" directly above the
	// reason this one could not start.
	events, _ := runKubectl("get", "events", "-n", workloadNamespace,
		"--field-selector", "involvedObject.uid="+uid,
		"-o", "jsonpath={range .items[*]}{.reason}: {.message}{\"; \"}{end}")

	return fmt.Sprintf("pod %s phase=%s state=%s events=[%s]",
		name, phase, strings.TrimSpace(state), strings.TrimSpace(events))
}

func deletePod(t *testing.T, name string) {
	t.Helper()

	// Best effort: a test that already failed should report its own reason,
	// not a cleanup error on top.
	_, _ = runKubectl("delete", "pod", name, "-n", workloadNamespace,
		"--ignore-not-found", "--force", "--grace-period=0")
}
