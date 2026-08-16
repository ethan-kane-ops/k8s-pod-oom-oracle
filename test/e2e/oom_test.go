//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oomTimeout bounds how long a kill may take to surface once the workload is
// already executing. It covers a sample interval, the balloon itself, and the
// hop from probe to HTTP API. Scheduling and container startup are deliberately
// outside it: waitForPodStarted absorbs those, so a timeout here means the
// daemon genuinely failed to report a kill.
const oomTimeout = 90 * time.Second

func TestDaemonIsWatchingTheNode(t *testing.T) {
	pod := daemonPod(t)
	got := daemonStatus(t, pod)

	if !got.Ready {
		t.Error("daemon reports not ready; readiness means the probe is attached and history is being kept")
	}
	// Anything else means the daemon is running but blind, which every other
	// test in this file would then fail on for a confusing reason.
	if got.TrackedCgroups == 0 {
		t.Error("daemon is tracking no cgroups")
	}
	switch got.Detector {
	case "ebpf", "poller":
	default:
		t.Errorf("detector = %q, want ebpf or poller", got.Detector)
	}
	// Kill counters and PSI both need the unified hierarchy. A v1 node would
	// silently degrade the whole product, so it is worth stating outright.
	if got.CgroupVersion != "v2" {
		t.Errorf("cgroupVersion = %q, want v2", got.CgroupVersion)
	}

	// Cluster correlation is what turns a UID into a name. It degrades quietly
	// by design, so the only way to know it is working is to ask.
	if got.Node == "" {
		t.Error("node is empty; the daemon did not learn its node name from the downward API")
	}
	if !got.PodCacheSynced {
		t.Error("pod cache is not synced; reports will identify pods by UID alone")
	}
	if got.PodsTracked == 0 {
		t.Error("pod cache holds no pods, yet this daemon's own pod runs on this node")
	}

	t.Logf("daemon: detector=%s cgroups=%s tracking=%d node=%s pods=%d version=%s",
		got.Detector, got.CgroupVersion, got.TrackedCgroups, got.Node, got.PodsTracked, got.Version)
}

// assertResolved checks that a report names the workload rather than merely
// identifying it by UID. This is the whole point of the pod informer: a
// developer reading a post-mortem recognises "team-a/api-7d9/app", not
// "11111111-2222-...".
func assertResolved(t *testing.T, found report, podName, containerName string) {
	t.Helper()

	if !found.Identity.Resolved {
		t.Error("identity is unresolved; the API server was not consulted or the pod was already gone")
		return
	}
	if found.Identity.Namespace != workloadNamespace {
		t.Errorf("namespace = %q, want %q", found.Identity.Namespace, workloadNamespace)
	}
	if found.Identity.PodName != podName {
		t.Errorf("podName = %q, want %q", found.Identity.PodName, podName)
	}
	if found.Identity.ContainerName != containerName {
		t.Errorf("containerName = %q, want %q", found.Identity.ContainerName, containerName)
	}
	if found.Identity.Image == "" {
		t.Error("image is empty; the container was matched but carried no image")
	}
}

// TestContainerOOMKilledOutright covers the case Kubernetes already reports:
// the container's only process exceeds the limit and the whole container dies.
// The daemon should still add what Kubernetes does not, which is the memory
// trajectory and the identity of the process.
func TestContainerOOMKilledOutright(t *testing.T) {
	const name = "oom-single-process"
	daemon := daemonPod(t)
	before := len(daemonReports(t, daemon))

	t.Cleanup(func() { deletePod(t, name) })
	applyPod(t, example(t, "single-process-killed.yaml"))

	uid := waitForPodUID(t, name)
	waitForPodStarted(t, name)
	found := waitForReport(t, daemon, name, before, uid)

	if !found.Victim.Known {
		t.Error("victim is unknown; the kill was seen but nothing was identified")
	}
	if found.LimitBytes != 128<<20 {
		t.Errorf("LimitBytes = %d, want %d", found.LimitBytes, 128<<20)
	}
	// The peak must reflect the balloon, not whatever the last sample caught
	// before it. This is the exact defect the eBPF work surfaced.
	if found.PeakBytes < 64<<20 {
		t.Errorf("PeakBytes = %d, implausibly low for a container killed at a 128MiB limit",
			found.PeakBytes)
	}
	if found.Identity.QoS == "" {
		t.Error("QoS is empty; the cgroup path was not parsed as a Kubernetes container")
	}
	assertResolved(t, found, name, "hog")

	t.Logf("report: source=%s pod=%s/%s/%s victim=%s pid=%d rss=%d peak=%d qos=%s",
		found.Source, found.Identity.Namespace, found.Identity.PodName,
		found.Identity.ContainerName, found.Victim.Comm, found.Victim.PID,
		found.Victim.RSSBytes, found.PeakBytes, found.Identity.QoS)
}

// TestMultiProcessContainerNamesTheVictim covers a container with several
// processes where one takes all the memory.
//
// This test used to assert that the container survived, which is what happens
// when memory.oom.group is 0. containerd sets it to 1, so the kernel kills every
// process in the cgroup and the container dies. The old assertion read
// `.status.phase` immediately after the report arrived and saw "Running" because
// the pod phase lags the kill by about a second; it passed while asserting
// something untrue.
//
// What holds on every runtime is that the daemon names which process the kernel
// chose, and that Kubernetes does not.
func TestMultiProcessContainerNamesTheVictim(t *testing.T) {
	const name = "oom-multi-process"
	daemon := daemonPod(t)
	before := len(daemonReports(t, daemon))

	t.Cleanup(func() { deletePod(t, name) })
	applyPod(t, example(t, "multi-process-survivor.yaml"))

	uid := waitForPodUID(t, name)
	waitForPodStarted(t, name)
	found := waitForReport(t, daemon, name, before, uid)

	if !found.Victim.Known {
		t.Fatal("victim is unknown; a child died and the daemon could not say which")
	}
	if found.Victim.Comm != "tail" {
		t.Errorf("victim = %q, want the ballooning child %q", found.Victim.Comm, "tail")
	}
	if found.Victim.NSPid == 0 {
		t.Error("NSPid is 0; the container-local PID is the one a developer recognises")
	}
	// Which processes appear is a race under group kill, so the listing's
	// contents are not asserted. That the victim is absent from it is not a
	// race: the daemon removes it by container-namespace PID, which is the same
	// number on both sides however the runtime is nested. Before that fix this
	// compared the kernel's global PID against a PID read from the node's /proc,
	// which cannot match inside kind, and the dead process was listed as though
	// it were alive.
	for _, proc := range found.Processes {
		if proc.NSPid == found.Victim.NSPid {
			t.Errorf("processes lists the victim (comm %q, nspid %d, host pid %d); "+
				"it is dead by definition and must be filtered out",
				proc.Comm, proc.NSPid, proc.PID)
		}
	}

	// The runtime's own setting is what decides whether the container could have
	// survived, so read it rather than inferring it from pod phase, which lags
	// the kill by about a second and reports "Running" for a container that is
	// already dead.
	// Both branches poll rather than sample once. Pod status trails the kill by
	// roughly a second, which is exactly the trap the old assertion fell into.
	groupKill := containerGroupKill(t, name)
	switch groupKill {
	case "1":
		// Every process in the cgroup is killed, so the container must die.
		eventually(t, 30*time.Second, "the container to be reported OOMKilled", func() error {
			if reason := terminatedReason(t, name); reason != "OOMKilled" {
				return fmt.Errorf("terminated reason = %q", reason)
			}
			return nil
		})
	case "0":
		// Only the victim is killed, so the container stays up and Kubernetes
		// reports nothing at all. This is the case the project exists for, and
		// the one the old assertion claimed to be testing everywhere.
		consistently(t, 10*time.Second, "the container to stay running", func() error {
			if reason := terminatedReason(t, name); reason != "" {
				return fmt.Errorf("terminated reason = %q, want none", reason)
			}
			return nil
		})
	default:
		t.Errorf("memory.oom.group = %q, want 0 or 1: the sample prints it at startup, "+
			"so an empty value means the workload or the log read is broken", groupKill)
	}
	t.Logf("runtime: memory.oom.group=%s terminated=%q", groupKill, terminatedReason(t, name))

	// Only one direction is safe to assert. The report's flag is set from a read
	// of the container's cgroup at report time, and under group kill that cgroup
	// is often already torn down, so false legitimately means "could not tell".
	// True is never a guess, so it must agree with the node.
	if found.GroupKill && groupKill != "1" {
		t.Errorf("report says groupKill=true but the container scope reports "+
			"memory.oom.group=%q", groupKill)
	}

	assertResolved(t, found, name, "app")

	t.Logf("report: source=%s pod=%s/%s/%s victim=%s hostpid=%d nspid=%d rss=%d inferred=%t processes=%d groupKill=%t",
		found.Source, found.Identity.Namespace, found.Identity.PodName,
		found.Identity.ContainerName, found.Victim.Comm, found.Victim.PID,
		found.Victim.NSPid, found.Victim.RSSBytes, found.Victim.Inferred,
		len(found.Processes), found.GroupKill)

	// The eBPF detector reads the victim out of the kernel, so it can state
	// things the poller can only guess at. Only hold it to that standard.
	if found.Source == "ebpf" {
		if found.Victim.Inferred {
			t.Error("a traced kill is marked inferred")
		}
		if found.Victim.RSSBytes == 0 {
			t.Error("traced victim has no resident memory recorded")
		}
		// The command line is deliberately not asserted. The kernel records
		// only a 15-character comm, so it can only come from reading /proc
		// while the victim is still dying, and that race is lost often enough
		// on a loaded node that requiring it would make this suite flaky about
		// something the report already presents as optional.
		if len(found.Victim.Cmdline) == 0 {
			t.Log("victim command line unavailable: /proc read lost the race with the kill")
		}
	}
}

// waitForPodUID waits for the pod to be admitted and returns its UID.
func waitForPodUID(t *testing.T, name string) string {
	t.Helper()

	var uid string
	eventually(t, 60*time.Second, "pod "+name+" to be admitted", func() error {
		out, err := runKubectl("get", "pod", name, "-n", workloadNamespace,
			"-o", "jsonpath={.metadata.uid}")
		if err != nil {
			return err
		}
		if out == "" {
			return fmt.Errorf("pod %s has no UID yet", name)
		}
		uid = out
		return nil
	})
	return uid
}

// waitForReport polls until a report appears for the given pod UID.
//
// Matching on the UID rather than on "the newest report" is what stops one
// test's kill satisfying another's assertion when they run against a shared
// daemon.
func waitForReport(t *testing.T, daemon, podName string, before int, uid string) report {
	t.Helper()

	var found report
	// The pod's own state is attached to the timeout because the two
	// explanations for a missing report look identical from here: the daemon
	// saw no kill, or the workload never produced one.
	eventuallyDiag(t, oomTimeout, "an OOM report for pod uid "+uid, func() error {
		reports := daemonReports(t, daemon)
		for _, candidate := range reports {
			if candidate.Identity.PodUID == uid {
				found = candidate
				return nil
			}
		}
		return fmt.Errorf("no report for uid %s among %d reports (%d before the test)",
			uid, len(reports), before)
	}, func() string { return podDiagnostics(t, podName) })
	return found
}

// containerGroupKill reports the pod's container-scope memory.oom.group, as "0",
// "1", or "" when it cannot be read.
//
// This is the setting that decides whether an OOM kills one process or the whole
// container, so it is the difference between two completely different expected
// outcomes. It is read from the node rather than assumed, because it varies by
// runtime: containerd sets 1, Docker leaves 0.
//
// The workload prints it at startup, which is cheaper and more honest than
// inspecting the node: a container can read its own cgroup, so the value comes
// from the very cgroup the kill will be charged to. Reading it from the node
// would mean a debug pod and another image to pull.
func containerGroupKill(t *testing.T, podName string) string {
	t.Helper()

	const prefix = "memory.oom.group="

	logs, err := runKubectl("logs", podName, "-n", workloadNamespace)
	if err != nil {
		t.Logf("reading %s logs for %s: %v", podName, prefix, err)
		return ""
	}
	for line := range strings.SplitSeq(logs, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return after
		}
	}
	return ""
}

// example loads a workload manifest from examples/workloads.
//
// The suite runs the published samples rather than its own copies of them. A
// sample that stops producing the report its header describes is a documentation
// bug, and this is what turns that into a test failure. It also means the two
// cannot drift, which they did as separate strings.
//
// The image is the one thing rewritten. Samples name a registry image so that
// `kubectl apply -f` works for a reader with no local build; the suite swaps in
// the tag preloaded into the kind node so no test waits on a registry. Nothing
// else is touched: the command, the limits and the container name are exactly
// what a reader would apply.
func example(t *testing.T, file string) string {
	t.Helper()

	path := filepath.Join("..", "..", "examples", "workloads", file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading example %s: %v", file, err)
	}

	manifest := string(raw)
	if !strings.Contains(manifest, exampleImage) {
		t.Fatalf("example %s does not use %s, so the suite cannot preload its image",
			file, exampleImage)
	}
	return strings.ReplaceAll(manifest, exampleImage, workloadImage)
}
