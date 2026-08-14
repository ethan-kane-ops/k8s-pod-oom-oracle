//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// oomTimeout bounds how long a kill may take to surface. A poll interval plus
// scheduling plus image pull on a cold CI runner fits comfortably inside it.
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
	const name = "e2e-single-process"
	daemon := daemonPod(t)
	before := len(daemonReports(t, daemon))

	t.Cleanup(func() { deletePod(t, name) })
	applyPod(t, singleProcessManifest(name))

	uid := waitForPodUID(t, name)
	found := waitForReport(t, daemon, before, uid)

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

// TestContainerSurvivesChildOOM is the case that justifies the project.
//
// A forked child is killed while the container's main process keeps running, so
// the pod never enters OOMKilled and kubectl reports nothing at all. The daemon
// must name the dead child and list the survivors.
func TestContainerSurvivesChildOOM(t *testing.T) {
	const name = "e2e-multi-process"
	daemon := daemonPod(t)
	before := len(daemonReports(t, daemon))

	t.Cleanup(func() { deletePod(t, name) })
	applyPod(t, multiProcessManifest(name))

	uid := waitForPodUID(t, name)
	found := waitForReport(t, daemon, before, uid)

	if !found.Victim.Known {
		t.Fatal("victim is unknown; a child died and the daemon could not say which")
	}
	if found.Victim.Comm != "tail" {
		t.Errorf("victim = %q, want the ballooning child %q", found.Victim.Comm, "tail")
	}
	if found.Victim.NSPid == 0 {
		t.Error("NSPid is 0; the container-local PID is the one a developer recognises")
	}
	// The whole point: the container is still up. Its other processes must
	// appear as survivors, and the victim must not.
	if len(found.Hogs) == 0 {
		t.Error("no surviving processes listed; the container should still be running")
	}
	for _, hog := range found.Hogs {
		if hog.PID == found.Victim.PID {
			t.Errorf("victim pid %d listed as a survivor", hog.PID)
		}
	}

	// Kubernetes itself saw nothing. Assert that, because it is the claim the
	// whole tool rests on.
	phase := kubectl(t, "get", "pod", name, "-n", workloadNamespace, "-o", "jsonpath={.status.phase}")
	if phase != "Running" {
		t.Errorf("pod phase = %q, want Running: the container should have survived its child's death", phase)
	}

	assertResolved(t, found, name, "app")

	t.Logf("report: source=%s pod=%s/%s/%s victim=%s hostpid=%d nspid=%d rss=%d inferred=%t survivors=%d",
		found.Source, found.Identity.Namespace, found.Identity.PodName,
		found.Identity.ContainerName, found.Victim.Comm, found.Victim.PID,
		found.Victim.NSPid, found.Victim.RSSBytes, found.Victim.Inferred, len(found.Hogs))

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
func waitForReport(t *testing.T, daemon string, before int, uid string) report {
	t.Helper()

	var found report
	eventually(t, oomTimeout, "an OOM report for pod uid "+uid, func() error {
		reports := daemonReports(t, daemon)
		for _, candidate := range reports {
			if candidate.Identity.PodUID == uid {
				found = candidate
				return nil
			}
		}
		return fmt.Errorf("no report for uid %s among %d reports (%d before the test)",
			uid, len(reports), before)
	})
	return found
}

// singleProcessManifest is one process that allocates past its limit, so the
// container itself is killed.
func singleProcessManifest(name string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
    - name: hog
      image: alpine:3.20
      command: ["sh", "-c"]
      # Climbs in steps so the sampler records a trajectory rather than a single
      # jump from idle to dead.
      args:
        - |
          sleep 3
          tail /dev/zero
      resources:
        limits:
          memory: 128Mi
        requests:
          memory: 64Mi
`, name)
}

// multiProcessManifest keeps PID 1 alive while a child is killed. The pod stays
// Running throughout, which is precisely why Kubernetes reports nothing.
func multiProcessManifest(name string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: alpine:3.20
      command: ["sh", "-c"]
      args:
        - |
          sleep 3600 &
          sleep 3600 &
          sleep 5
          ( tail /dev/zero ) &
          wait %%3
          sleep 3600
      resources:
        limits:
          memory: 512Mi
        requests:
          memory: 64Mi
`, name)
}
