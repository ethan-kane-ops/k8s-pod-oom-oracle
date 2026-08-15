package correlate

import (
	"strings"
	"testing"
)

// ParseCgroupPath decides whether a kill belongs to Kubernetes at all, and
// which pod. Every layout it handles came from a real runtime: systemd slices,
// cgroupfs directories, the kubelet's own cgroup root, and the flattened
// parent names systemd produces. Getting it wrong silently misattributes a
// kill; panicking on it takes the daemon down.
func FuzzParseCgroupPath(f *testing.F) {
	seeds := []string{
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pode257c815_3e31_4c9e_9d1a_2b3c4d5e6f70.slice/cri-containerd-abc123.scope",
		"/kubepods/besteffort/pode257c815-3e31-4c9e-9d1a-2b3c4d5e6f70/abc123",
		"/kubepods.slice/kubepods-pode257c815_3e31_4c9e_9d1a_2b3c4d5e6f70.slice/docker-abc123.scope",
		// A kubelet with its own cgroup root: systemd flattens the parent slice
		// into each child's name, so the prefix is not simply "kubepods-".
		"/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-pode257c815_3e31_4c9e_9d1a_2b3c4d5e6f70.slice/cri-containerd-abc123.scope",
		"/system.slice/containerd.service",
		"/",
		"",
		"//////",
		"/kubepods.slice/kubepods-burstable.slice",
		"/kubepods.slice/kubepods-burstable-podNOTAUUID.slice/cri-containerd-abc.scope",
		"/kubepods.slice/kubepods-burstable-pod.slice/cri-containerd-.scope",
		// Pod-level slices, where anything charged to the pod rather than to
		// one of its containers is accounted.
		"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pode257c815_3e31_4c9e_9d1a_2b3c4d5e6f70.slice",
		"/kubepods/besteffort/pode257c815-3e31-4c9e-9d1a-2b3c4d5e6f70",
		"/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/kubelet-kubepods-burstable-pode257c815_3e31_4c9e_9d1a_2b3c4d5e6f70.slice",
		// Pod-shaped names that belong to the host, not to the kubelet.
		"/machine.slice/podman",
		"/somewhere/pode257c815-3e31-4c9e-9d1a-2b3c4d5e6f70",
		strings.Repeat("/a", 512),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		scope, ok := ParseCgroupPath(path)
		if !ok {
			return
		}
		// Every accepted path must name a pod, whichever level it sits at:
		// the UID is what every report is correlated on, and reporting ok
		// without one produces a report attached to nothing.
		if scope.PodUID == "" {
			t.Errorf("ParseCgroupPath(%q) reported a match with no pod UID: %+v", path, scope)
		}

		switch scope.Kind {
		case ScopeContainer:
			if scope.ContainerID == "" {
				t.Errorf("ParseCgroupPath(%q) reported a container with no container ID: %+v", path, scope)
			}
		case ScopePod:
			// A pod scope names no container by definition. Carrying an ID
			// here would pin a shared allocation on whichever container the
			// parser happened to reach for.
			if scope.ContainerID != "" {
				t.Errorf("ParseCgroupPath(%q) reported a pod scope carrying container ID %q: %+v",
					path, scope.ContainerID, scope)
			}
		default:
			t.Errorf("ParseCgroupPath(%q) reported an unknown scope kind %q: %+v", path, scope.Kind, scope)
		}
	})
}
