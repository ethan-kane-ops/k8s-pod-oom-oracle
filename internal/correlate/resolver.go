package correlate

import (
	"maps"
	"strings"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

// ContainerInfo is what the API server knows about one container in a pod.
type ContainerInfo struct {
	// Name is the container's name in the pod spec.
	Name string `json:"name"`
	// Image is the image reference it was started from.
	Image string `json:"image"`
}

// PodInfo is the cluster identity behind a pod UID.
type PodInfo struct {
	// Namespace and Name identify the pod to a human.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Node is the node the pod is scheduled on.
	Node string `json:"node"`
	// Containers maps bare container ID to container details. Runtime scheme
	// prefixes such as "containerd://" are stripped by the lookup, since a
	// cgroup path carries the bare ID.
	Containers map[string]ContainerInfo `json:"containers"`
}

// PodLookup resolves a pod UID to its cluster identity.
//
// The daemon backs this with a client-go informer scoped to its own node. Tests
// back it with a map. Keeping it an interface is what allows the correlation
// pipeline to be tested end to end without an API server.
type PodLookup interface {
	LookupPod(podUID string) (PodInfo, bool)
}

// Identity is a container resolved as far as the available information allows.
//
// Scope is always populated, since it comes from the cgroup path alone. The
// cluster fields are populated only when the pod was found: a container that
// has just been deleted, or one belonging to a pod the daemon cannot see, still
// produces a usable Identity with an empty Namespace.
type Identity struct {
	Scope `json:",inline"`

	// Namespace and PodName come from the API server.
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`
	// ContainerName and Image identify the container within the pod.
	ContainerName string `json:"containerName"`
	Image         string `json:"image"`
	// Resolved reports whether the cluster fields were populated.
	Resolved bool `json:"resolved"`
}

// String renders the identity the way a post-mortem header reads.
func (i Identity) String() string {
	if !i.Resolved {
		return "pod " + i.PodUID + " (unresolved)"
	}
	if i.ContainerName == "" {
		return i.Namespace + "/" + i.PodName
	}
	return i.Namespace + "/" + i.PodName + "/" + i.ContainerName
}

// Resolver joins a cgroup path to a full Kubernetes identity.
type Resolver struct {
	pods PodLookup
}

// NewResolver builds a Resolver over a pod lookup. A nil lookup is allowed and
// yields identities carrying only what the cgroup path proves, which is how the
// CLI works when it has no cluster access.
func NewResolver(pods PodLookup) *Resolver {
	return &Resolver{pods: pods}
}

// Resolve maps a cgroup path to an identity. It reports false only when the
// path is not a Kubernetes container cgroup at all.
func (r *Resolver) Resolve(cgroupPath string) (Identity, bool) {
	scope, ok := ParseCgroupPath(cgroupPath)
	if !ok {
		return Identity{}, false
	}

	identity := Identity{Scope: scope}
	if r.pods == nil {
		return identity, true
	}

	pod, found := r.pods.LookupPod(scope.PodUID)
	if !found {
		return identity, true
	}

	identity.Namespace = pod.Namespace
	identity.PodName = pod.Name
	identity.Resolved = true

	if container, found := pod.Containers[scope.ContainerID]; found {
		identity.ContainerName = container.Name
		identity.Image = container.Image
	}

	return identity, true
}

// ResolveProcess maps a process to the container it belongs to, using the
// cgroup path recorded on the process.
func (r *Resolver) ResolveProcess(proc procfs.Process) (Identity, bool) {
	if proc.CgroupPath == "" {
		return Identity{}, false
	}
	return r.Resolve(proc.CgroupPath)
}

// StripRuntimePrefix removes the runtime scheme from a Kubernetes container ID.
//
// The API server reports IDs as "containerd://<hex>" while a cgroup path
// carries the bare hex, so they cannot be compared without this.
func StripRuntimePrefix(containerID string) string {
	if _, id, found := strings.Cut(containerID, "://"); found {
		return id
	}
	return containerID
}

// MapPodLookup is an in-memory PodLookup, used by tests and by the CLI when
// pod metadata is supplied from a file rather than an API server.
type MapPodLookup map[string]PodInfo

// LookupPod satisfies PodLookup.
func (m MapPodLookup) LookupPod(podUID string) (PodInfo, bool) {
	pod, ok := m[podUID]
	return pod, ok
}

// Clone returns a deep copy, so callers can mutate a snapshot safely.
func (m MapPodLookup) Clone() MapPodLookup {
	out := make(MapPodLookup, len(m))
	for uid, pod := range m {
		copied := pod
		copied.Containers = maps.Clone(pod.Containers)
		out[uid] = copied
	}
	return out
}
