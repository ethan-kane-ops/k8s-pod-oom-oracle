package correlate

import "testing"

// Realistic identifiers reused across cases.
const (
	podUID       = "3f0e2b6c-1a2b-4c3d-9e8f-0a1b2c3d4e5f"
	podUIDEscape = "3f0e2b6c_1a2b_4c3d_9e8f_0a1b2c3d4e5f"
	containerID  = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"
)

// TestParseCgroupPathKubeletCgroupRoot covers a kubelet run with a cgroup root
// of its own, which is what kind does and what any cluster passing
// --cgroup-root gets. systemd flattens the parent slice into each child's name,
// so every segment gains a "kubelet-" prefix.
func TestParseCgroupPathKubeletCgroupRoot(t *testing.T) {
	t.Parallel()

	const path = "/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/" +
		"kubelet-kubepods-burstable-pod022353b0_bae5_4670_a5d8_c92a05e6f7ca.slice/" +
		"cri-containerd-55ba2819dd45a03ed673c4a055d058ae33c15a6e0d80f045a471d6ee3b36eeb0.scope"

	scope, ok := ParseCgroupPath(path)
	if !ok {
		t.Fatalf("ParseCgroupPath(%q) reported no Kubernetes container", path)
	}
	if want := "022353b0-bae5-4670-a5d8-c92a05e6f7ca"; scope.PodUID != want {
		t.Errorf("PodUID = %q, want %q", scope.PodUID, want)
	}
	if want := "55ba2819dd45a03ed673c4a055d058ae33c15a6e0d80f045a471d6ee3b36eeb0"; scope.ContainerID != want {
		t.Errorf("ContainerID = %q, want %q", scope.ContainerID, want)
	}
	// The bug this covers: the QoS segment is "kubelet-kubepods-burstable",
	// not "kubepods-burstable", and matching on a prefix reported Unknown.
	if scope.QoS != QoSBurstable {
		t.Errorf("QoS = %q, want %q", scope.QoS, QoSBurstable)
	}
	if scope.Driver != DriverSystemd {
		t.Errorf("Driver = %q, want %q", scope.Driver, DriverSystemd)
	}
	if scope.Kind != ScopeContainer {
		t.Errorf("Kind = %q, want %q", scope.Kind, ScopeContainer)
	}
}

// TestParseCgroupPathPodScopes covers the level above a container: the pod
// slice itself, which holds whatever is charged to the pod rather than to one
// of its containers. The pages of a memory-backed emptyDir land there, so a
// pod that fills /dev/shm is killed on this cgroup and no other.
//
// Every case here was previously rejected as unparseable, which is how such
// kills came to be dropped without so much as a warning.
func TestParseCgroupPathPodScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want Scope
	}{
		{
			name: "systemd burstable",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice",
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeUnknown},
		},
		{
			name: "systemd besteffort",
			path: "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + podUIDEscape + ".slice",
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSBestEffort, Driver: DriverSystemd, Runtime: RuntimeUnknown},
		},
		{
			name: "systemd guaranteed omits the qos segment",
			path: "/kubepods.slice/kubepods-pod" + podUIDEscape + ".slice",
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSGuaranteed, Driver: DriverSystemd, Runtime: RuntimeUnknown},
		},
		{
			// What a kind node actually produces: the kubelet runs under a
			// cgroup root of its own, and systemd flattens the parent slice
			// into every child's name.
			name: "kubelet cgroup root",
			path: "/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice/" +
				"kubelet-kubepods-burstable-pod" + podUIDEscape + ".slice",
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeUnknown},
		},
		{
			name: "cgroupfs burstable",
			path: "/kubepods/burstable/pod" + podUID,
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSBurstable, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "cgroupfs guaranteed omits the qos segment",
			path: "/kubepods/pod" + podUID,
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSGuaranteed, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "trailing slash tolerated",
			path: "/kubepods/burstable/pod" + podUID + "/",
			want: Scope{PodUID: podUID, Kind: ScopePod, QoS: QoSBurstable, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseCgroupPath(tt.path)
			if !ok {
				t.Fatalf("ParseCgroupPath(%q) reported not a Kubernetes cgroup", tt.path)
			}

			tt.want.CgroupPath = tt.path
			if got != tt.want {
				t.Errorf("ParseCgroupPath() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseCgroupPathKubernetesContainers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want Scope
	}{
		{
			name: "systemd burstable containerd",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeContainerd},
		},
		{
			name: "systemd besteffort containerd",
			path: "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBestEffort, Driver: DriverSystemd, Runtime: RuntimeContainerd},
		},
		{
			name: "systemd guaranteed omits the qos segment",
			path: "/kubepods.slice/kubepods-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSGuaranteed, Driver: DriverSystemd, Runtime: RuntimeContainerd},
		},
		{
			name: "systemd cri-o",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/crio-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeCRIO},
		},
		{
			name: "systemd docker",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/docker-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeDocker},
		},
		{
			name: "systemd bare containerd prefix",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/containerd-" + containerID + ".scope",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverSystemd, Runtime: RuntimeContainerd},
		},
		{
			name: "cgroupfs burstable",
			path: "/kubepods/burstable/pod" + podUID + "/" + containerID,
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "cgroupfs besteffort",
			path: "/kubepods/besteffort/pod" + podUID + "/" + containerID,
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBestEffort, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "cgroupfs guaranteed omits the qos segment",
			path: "/kubepods/pod" + podUID + "/" + containerID,
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSGuaranteed, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "trailing slash tolerated",
			path: "/kubepods/burstable/pod" + podUID + "/" + containerID + "/",
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
		{
			name: "duplicate separators tolerated",
			path: "//kubepods//burstable//pod" + podUID + "//" + containerID,
			want: Scope{PodUID: podUID, ContainerID: containerID, Kind: ScopeContainer, QoS: QoSBurstable, Driver: DriverCgroupfs, Runtime: RuntimeUnknown},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseCgroupPath(tt.path)
			if !ok {
				t.Fatalf("ParseCgroupPath(%q) reported not a Kubernetes container", tt.path)
			}

			tt.want.CgroupPath = tt.path
			if got != tt.want {
				t.Errorf("ParseCgroupPath() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestParseCgroupPathRejectsNonKubernetes guards the filter that keeps host
// noise out of the daemon. The probes see every OOM kill on the node, so a
// false positive here would attribute a systemd service crash to a pod.
//
// The pod-shaped paths matter most. A container segment is a long hex ID that
// identifies itself, but a pod segment is only "pod" followed by anything, so
// accepting pod-level cgroups means every podman and podinfo directory on the
// host now looks like somebody's pod. What keeps them out is the requirement of
// a kubepods ancestor, and these cases are what proves it still holds.
func TestParseCgroupPathRejectsNonKubernetes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "root", path: "/"},
		{name: "single segment", path: "/kubepods.slice"},
		{name: "kubepods root alone", path: "/kubepods"},
		{name: "systemd service", path: "/system.slice/docker.service"},
		{name: "systemd user session", path: "/user.slice/user-1000.slice/session-3.scope"},
		{name: "plain docker container", path: "/docker/" + containerID},
		{name: "kubelet own slice", path: "/system.slice/kubelet.service"},
		{name: "qos level slice holds no pod", path: "/kubepods.slice/kubepods-burstable.slice"},
		{name: "cgroupfs qos level holds no pod", path: "/kubepods/burstable"},
		{name: "podman is not a pod", path: "/machine.slice/podman"},
		{name: "podinfo is not a pod", path: "/system.slice/podinfo.slice"},
		{name: "pod shaped but outside the kubepods tree", path: "/somewhere/pod" + podUID},
		{
			name: "systemd scope with an unrecognised runtime prefix",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/mystery-" + containerID + ".scope",
		},
		{
			name: "systemd container segment missing its scope suffix",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID,
		},
		{
			name: "empty pod uid",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod.slice/cri-containerd-" + containerID + ".scope",
		},
		{
			name: "empty container id",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/cri-containerd-.scope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got, ok := ParseCgroupPath(tt.path); ok {
				t.Errorf("ParseCgroupPath(%q) = %+v, want it rejected as non-Kubernetes", tt.path, got)
			}
		})
	}
}

// TestParseCgroupPathUnescapesOnlySystemdUIDs pins the asymmetry between the
// drivers. systemd escapes the UID's dashes to underscores; cgroupfs does not.
// Reversing the substitution on a cgroupfs path would corrupt any UID that
// legitimately contains an underscore, as static pod UIDs can.
// TestInKubepodsTree covers the daemon's test for its own blind spots. It is
// deliberately broader than ParseCgroupPath: a path can lie inside the kubelet's
// hierarchy and still be unparseable, and that combination is precisely the case
// worth warning about, because a report someone is waiting for was just dropped.
func TestInKubepodsTree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "container scope", path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope", want: true},
		{name: "pod slice", path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice", want: true},
		{name: "qos level", path: "/kubepods.slice/kubepods-burstable.slice", want: true},
		{name: "kubepods root", path: "/kubepods.slice", want: true},
		{name: "cgroupfs tree", path: "/kubepods/burstable/pod" + podUID, want: true},
		{
			// The reason this is a containment test and not a prefix test: a
			// kubelet with its own cgroup root has no segment that starts with
			// "kubepods", because systemd flattens the parent slice into every
			// child's name.
			name: "kubelet cgroup root",
			path: "/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-burstable.slice",
			want: true,
		},
		{
			// Inside the tree but unparseable, which is the case the daemon
			// exists to shout about.
			name: "unrecognised runtime prefix",
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUIDEscape + ".slice/mystery-" + containerID + ".scope",
			want: true,
		},
		{name: "empty", path: ""},
		{name: "root", path: "/"},
		{name: "systemd service", path: "/system.slice/docker.service"},
		{name: "kubelet own service", path: "/system.slice/kubelet.service"},
		{name: "plain docker container", path: "/docker/" + containerID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := InKubepodsTree(tt.path); got != tt.want {
				t.Errorf("InKubepodsTree(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestParseCgroupPathUnescapesOnlySystemdUIDs(t *testing.T) {
	t.Parallel()

	systemd, ok := ParseCgroupPath("/kubepods.slice/kubepods-pod" + podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope")
	if !ok {
		t.Fatal("systemd path was rejected")
	}
	if systemd.PodUID != podUID {
		t.Errorf("systemd PodUID = %q, want the underscores restored to dashes as %q", systemd.PodUID, podUID)
	}

	const literalUnderscoreUID = "static_pod_uid_1234"
	cgroupfs, ok := ParseCgroupPath("/kubepods/pod" + literalUnderscoreUID + "/" + containerID)
	if !ok {
		t.Fatal("cgroupfs path was rejected")
	}
	if cgroupfs.PodUID != literalUnderscoreUID {
		t.Errorf("cgroupfs PodUID = %q, want %q left untouched", cgroupfs.PodUID, literalUnderscoreUID)
	}
}

func TestParseQoSFallsBackToUnknownOutsideKubepods(t *testing.T) {
	t.Parallel()

	// A pod-shaped path that is not under a kubepods root must not be claimed
	// as Guaranteed.
	got, ok := ParseCgroupPath("/somewhere/pod" + podUID + "/" + containerID)
	if !ok {
		t.Fatal("path was rejected")
	}
	if got.QoS != QoSUnknown {
		t.Errorf("QoS = %q, want %q outside a kubepods tree", got.QoS, QoSUnknown)
	}
}
