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
		// A path that claims to be a Kubernetes container must carry the two
		// identifiers everything downstream keys on. Reporting ok with either
		// missing would produce a report correlated to nothing.
		if scope.PodUID == "" {
			t.Errorf("ParseCgroupPath(%q) reported a match with no pod UID: %+v", path, scope)
		}
		if scope.ContainerID == "" {
			t.Errorf("ParseCgroupPath(%q) reported a match with no container ID: %+v", path, scope)
		}
	})
}
