package correlate

import (
	"testing"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

const systemdContainerPath = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" +
	podUIDEscape + ".slice/cri-containerd-" + containerID + ".scope"

func testLookup() MapPodLookup {
	return MapPodLookup{
		podUID: {
			Namespace: "default",
			Name:      "payment-api-6d5f78",
			Node:      "node-1",
			Containers: map[string]ContainerInfo{
				containerID: {Name: "web-server", Image: "payment:v1.2.0"},
			},
		},
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lookup       PodLookup
		path         string
		wantOK       bool
		wantResolved bool
		want         Identity
	}{
		{
			name:         "fully resolved container",
			lookup:       testLookup(),
			path:         systemdContainerPath,
			wantOK:       true,
			wantResolved: true,
			want: Identity{
				Namespace: "default", PodName: "payment-api-6d5f78",
				ContainerName: "web-server", Image: "payment:v1.2.0",
			},
		},
		{
			name:   "unknown pod still yields the cgroup-derived scope",
			lookup: MapPodLookup{},
			path:   systemdContainerPath,
			wantOK: true,
		},
		{
			name:   "nil lookup yields the cgroup-derived scope",
			lookup: nil,
			path:   systemdContainerPath,
			wantOK: true,
		},
		{
			name:   "non-kubernetes path is rejected outright",
			lookup: testLookup(),
			path:   "/system.slice/docker.service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := NewResolver(tt.lookup).Resolve(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("Resolve() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}

			if got.Resolved != tt.wantResolved {
				t.Errorf("Resolved = %v, want %v", got.Resolved, tt.wantResolved)
			}
			// The scope is always populated from the path alone.
			if got.PodUID != podUID {
				t.Errorf("PodUID = %q, want %q", got.PodUID, podUID)
			}
			if got.ContainerID != containerID {
				t.Errorf("ContainerID = %q, want %q", got.ContainerID, containerID)
			}

			if tt.wantResolved {
				if got.Namespace != tt.want.Namespace || got.PodName != tt.want.PodName {
					t.Errorf("pod = %s/%s, want %s/%s", got.Namespace, got.PodName, tt.want.Namespace, tt.want.PodName)
				}
				if got.ContainerName != tt.want.ContainerName || got.Image != tt.want.Image {
					t.Errorf("container = %s (%s), want %s (%s)",
						got.ContainerName, got.Image, tt.want.ContainerName, tt.want.Image)
				}
			}
		})
	}
}

// TestResolveUnknownContainerInKnownPod covers a sandbox ("pause") container:
// the pod resolves, but the container ID is not in its ContainerStatuses.
func TestResolveUnknownContainerInKnownPod(t *testing.T) {
	t.Parallel()

	const sandboxID = "0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
	path := "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" +
		podUIDEscape + ".slice/cri-containerd-" + sandboxID + ".scope"

	got, ok := NewResolver(testLookup()).Resolve(path)
	if !ok {
		t.Fatal("Resolve() reported not a Kubernetes container")
	}
	if !got.Resolved {
		t.Error("Resolved = false; the pod itself was found and should be reported")
	}
	if got.ContainerName != "" {
		t.Errorf("ContainerName = %q, want empty for an unrecognised container ID", got.ContainerName)
	}
	if got.PodName != "payment-api-6d5f78" {
		t.Errorf("PodName = %q, want the pod still identified", got.PodName)
	}
}

func TestResolveProcess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		proc   procfs.Process
		wantOK bool
	}{
		{
			name:   "containerised process",
			proc:   procfs.Process{PID: 28145, CgroupPath: systemdContainerPath},
			wantOK: true,
		},
		{
			name: "host process",
			proc: procfs.Process{PID: 1, CgroupPath: "/init.scope"},
		},
		{
			name: "process with no cgroup path recorded",
			proc: procfs.Process{PID: 42},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := NewResolver(testLookup()).ResolveProcess(tt.proc)
			if ok != tt.wantOK {
				t.Fatalf("ResolveProcess() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.PodName != "payment-api-6d5f78" {
				t.Errorf("PodName = %q, want the pod resolved", got.PodName)
			}
		})
	}
}

func TestStripRuntimePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "containerd://" + containerID, want: containerID},
		{input: "cri-o://" + containerID, want: containerID},
		{input: "docker://" + containerID, want: containerID},
		{input: containerID, want: containerID},
		{input: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			if got := StripRuntimePrefix(tt.input); got != tt.want {
				t.Errorf("StripRuntimePrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIdentityString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		identity Identity
		want     string
	}{
		{
			name: "fully resolved",
			identity: Identity{
				Namespace: "default", PodName: "payment-api", ContainerName: "web", Resolved: true,
			},
			want: "default/payment-api/web",
		},
		{
			name:     "pod resolved but container unknown",
			identity: Identity{Namespace: "default", PodName: "payment-api", Resolved: true},
			want:     "default/payment-api",
		},
		{
			name:     "unresolved falls back to the uid",
			identity: Identity{Scope: Scope{PodUID: podUID}},
			want:     "pod " + podUID + " (unresolved)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.identity.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMapPodLookupClone(t *testing.T) {
	t.Parallel()

	original := testLookup()
	clone := original.Clone()

	// Mutating the clone's nested map must not reach the original.
	clone[podUID].Containers[containerID] = ContainerInfo{Name: "mutated", Image: "bad"}

	got, ok := original.LookupPod(podUID)
	if !ok {
		t.Fatal("LookupPod() on the original reported not found")
	}
	if got.Containers[containerID].Name != "web-server" {
		t.Errorf("original container name = %q, want %q; Clone must deep-copy",
			got.Containers[containerID].Name, "web-server")
	}
}
