// Package correlate maps kernel-level cgroup paths onto Kubernetes identities.
//
// A cgroup path is the only link the kernel gives between an OOM kill and the
// pod that owns it. Parsing it is pure string work, which keeps the riskiest
// correlation logic fully testable without a cluster.
package correlate

import (
	"strings"
)

// QoSClass is the Kubernetes quality-of-service tier a pod was scheduled under.
// It is encoded in the cgroup path because the kubelet nests pods by QoS.
type QoSClass string

// Quality-of-service tiers.
const (
	QoSGuaranteed QoSClass = "Guaranteed"
	QoSBurstable  QoSClass = "Burstable"
	QoSBestEffort QoSClass = "BestEffort"
	QoSUnknown    QoSClass = "Unknown"
)

// Driver is the kubelet cgroup driver that produced a path.
type Driver string

// Cgroup drivers.
const (
	// DriverSystemd renders paths as .slice and .scope units with escaped names.
	DriverSystemd Driver = "systemd"
	// DriverCgroupfs renders paths as plain nested directories.
	DriverCgroupfs Driver = "cgroupfs"
	DriverUnknown  Driver = "unknown"
)

// Runtime is the container runtime that created a container cgroup.
type Runtime string

// Container runtimes.
const (
	RuntimeContainerd Runtime = "containerd"
	RuntimeCRIO       Runtime = "cri-o"
	RuntimeDocker     Runtime = "docker"
	RuntimeUnknown    Runtime = "unknown"
)

// ScopeKind is the level of the kubelet's tree that a cgroup path names.
//
// The distinction matters because the kernel charges memory at both levels. A
// container's own allocations land on its scope; anything shared between the
// pod's containers lands on the pod slice above it, most commonly the pages of
// a memory-backed emptyDir. A kill on the pod slice names no container, and
// treating that as unparseable is how such kills came to be dropped entirely.
type ScopeKind string

// Levels of the kubelet's tree.
const (
	// ScopeContainer is one container's own cgroup.
	ScopeContainer ScopeKind = "container"
	// ScopePod is the slice holding every container in a pod, and everything
	// charged to the pod rather than to one of them.
	ScopePod ScopeKind = "pod"
)

// Scope is the Kubernetes identity encoded in a cgroup path.
//
// It carries only what the path itself proves. Resolving PodUID to a namespace
// and name requires the API server, which is a separate step.
type Scope struct {
	// PodUID is the pod's UID in canonical dashed form.
	PodUID string `json:"podUID"`
	// ContainerID is the runtime's container ID, without its runtime prefix.
	// It is empty for ScopePod, which belongs to no single container.
	ContainerID string `json:"containerID"`
	// Kind is the level of the tree the path names.
	Kind ScopeKind `json:"kind"`
	// QoS is the tier encoded in the path.
	QoS QoSClass `json:"qos"`
	// Driver is the cgroup driver that produced the path.
	Driver Driver `json:"driver"`
	// Runtime is the container runtime that owns the container.
	Runtime Runtime `json:"runtime"`
	// CgroupPath is the path this scope was parsed from.
	CgroupPath string `json:"cgroupPath"`
}

// runtimePrefixes maps the container-ID prefix each runtime uses onto its name.
// Order matters: cri-containerd- must be tested before containerd-.
var runtimePrefixes = []struct {
	prefix  string
	runtime Runtime
}{
	{prefix: "cri-containerd-", runtime: RuntimeContainerd},
	{prefix: "containerd-", runtime: RuntimeContainerd},
	{prefix: "crio-", runtime: RuntimeCRIO},
	{prefix: "docker-", runtime: RuntimeDocker},
}

// ParseCgroupPath extracts the Kubernetes identity from a cgroup path.
//
// It reports false for anything outside the kubelet's pod hierarchy: system
// services, the kubelet's own slice, and the QoS levels that hold no pod.
// Callers use that to filter out host noise, which matters because the daemon
// sees every OOM kill on the node, not only the ones in pods.
//
// Both kubelet cgroup drivers are handled, at both levels of the tree:
//
//	systemd   /kubepods.slice/kubepods-burstable.slice/
//	          kubepods-burstable-pod<uid>.slice[/cri-containerd-<id>.scope]
//	cgroupfs  /kubepods/burstable/pod<uid>[/<id>]
//
// A path ending at the pod slice yields ScopePod with no container ID. Rejecting
// those instead is what dropped every kill charged to a pod rather than to one
// of its containers, which is where a memory-backed emptyDir is accounted.
//
// Guaranteed pods omit the QoS segment under both drivers, since the kubelet
// nests them directly beneath the kubepods root.
func ParseCgroupPath(cgroupPath string) (Scope, bool) {
	segments := splitPath(cgroupPath)
	if len(segments) == 0 {
		return Scope{}, false
	}

	scope, ok := parseContainerScope(segments)
	if !ok {
		scope, ok = parsePodScope(segments)
	}
	if !ok {
		return Scope{}, false
	}

	scope.CgroupPath = cgroupPath
	return scope, true
}

// parseContainerScope reads a path whose last two segments are a pod and one of
// its containers.
func parseContainerScope(segments []string) (Scope, bool) {
	if len(segments) < 2 {
		return Scope{}, false
	}

	// The container segment is always last, the pod segment second to last.
	podUID, driver, ok := parsePodSegment(segments[len(segments)-2])
	if !ok {
		return Scope{}, false
	}

	containerID, runtime, ok := parseContainerSegment(segments[len(segments)-1], driver)
	if !ok {
		return Scope{}, false
	}

	return Scope{
		PodUID:      podUID,
		ContainerID: containerID,
		Kind:        ScopeContainer,
		QoS:         parseQoS(segments, driver),
		Driver:      driver,
		Runtime:     runtime,
	}, true
}

// parsePodScope reads a path that ends at a pod slice and holds no container.
//
// The kubepods guard is stricter than anything the container case needs. A
// container segment is a long hex ID that pins a path down by itself, whereas a
// pod segment is only "pod" followed by a UID: without an ancestor naming the
// kubelet's tree, any directory called podman or podinfo would parse as
// somebody's pod and misattribute a host kill to it.
func parsePodScope(segments []string) (Scope, bool) {
	if !inKubepodsTree(segments) {
		return Scope{}, false
	}

	podUID, driver, ok := parsePodSegment(segments[len(segments)-1])
	if !ok {
		return Scope{}, false
	}

	return Scope{
		PodUID:  podUID,
		Kind:    ScopePod,
		QoS:     parseQoS(segments, driver),
		Driver:  driver,
		Runtime: RuntimeUnknown,
	}, true
}

// InKubepodsTree reports whether a cgroup path lies inside the kubelet's pod
// hierarchy, whether or not it parses into an identity.
//
// The daemon uses it to tell node noise from its own blind spots. A kill it
// cannot parse is routine outside this tree and a lost report inside it.
func InKubepodsTree(cgroupPath string) bool {
	return inKubepodsTree(splitPath(cgroupPath))
}

// inKubepodsTree reports whether any segment names the kubelet's pod hierarchy.
//
// Containment rather than a prefix: a kubelet given its own cgroup root produces
// "kubelet-kubepods.slice", because systemd flattens the parent slice into every
// child's name.
func inKubepodsTree(segments []string) bool {
	for _, segment := range segments {
		if strings.Contains(segment, "kubepods") {
			return true
		}
	}
	return false
}

// parsePodSegment extracts a pod UID and infers the driver that wrote it.
//
// systemd:  kubepods-burstable-pod<uid>.slice  (UID dashes escaped to _)
//
//	or kubepods-pod<uid>.slice for Guaranteed
//
// cgroupfs: pod<uid>                           (UID keeps its dashes)
func parsePodSegment(segment string) (podUID string, driver Driver, ok bool) {
	if suffix, found := strings.CutSuffix(segment, ".slice"); found {
		// Anchor on "-pod" rather than "pod": a bare search would match inside
		// "kubepods" and parse the QoS-level slice as if it held a pod. systemd
		// escapes the UID's dashes to underscores, so the last "-pod" in the
		// name is unambiguously the separator.
		var uid string
		switch {
		case strings.Contains(suffix, "-pod"):
			uid = suffix[strings.LastIndex(suffix, "-pod")+len("-pod"):]
		case strings.HasPrefix(suffix, "pod"):
			uid = strings.TrimPrefix(suffix, "pod")
		default:
			return "", DriverUnknown, false
		}
		if uid == "" {
			return "", DriverUnknown, false
		}
		// systemd escapes dashes to underscores in unit names, so reversing the
		// substitution recovers the real UID. This is only correct for systemd
		// paths, which is why it does not run in the cgroupfs branch.
		return strings.ReplaceAll(uid, "_", "-"), DriverSystemd, true
	}

	uid, found := strings.CutPrefix(segment, "pod")
	if !found || uid == "" {
		return "", DriverUnknown, false
	}
	return uid, DriverCgroupfs, true
}

// parseContainerSegment extracts a container ID and its runtime.
//
// Under systemd the segment is <runtime-prefix><id>.scope. Under cgroupfs it is
// the bare container ID, which carries no runtime hint.
func parseContainerSegment(segment string, driver Driver) (containerID string, runtime Runtime, ok bool) {
	if driver == DriverSystemd {
		body, found := strings.CutSuffix(segment, ".scope")
		if !found {
			return "", RuntimeUnknown, false
		}
		for _, candidate := range runtimePrefixes {
			if id, found := strings.CutPrefix(body, candidate.prefix); found {
				if id == "" {
					return "", RuntimeUnknown, false
				}
				return id, candidate.runtime, true
			}
		}
		return "", RuntimeUnknown, false
	}

	// A cgroupfs container segment is the raw ID. Reject anything that still
	// looks structural, so a pod-level cgroup is not mistaken for a container.
	if segment == "" || strings.Contains(segment, ".") || strings.HasPrefix(segment, "pod") {
		return "", RuntimeUnknown, false
	}
	return segment, RuntimeUnknown, true
}

// parseQoS reads the tier from the path. The kubelet nests Burstable and
// BestEffort pods under a QoS segment and Guaranteed pods directly under the
// kubepods root, so the absence of a QoS segment is itself the signal.
func parseQoS(segments []string, driver Driver) QoSClass {
	for _, segment := range segments {
		name := segment
		if driver == DriverSystemd {
			name = strings.TrimSuffix(name, ".slice")
			// A kubelet given its own cgroup root produces
			// "kubelet-kubepods-burstable.slice", because systemd flattens the
			// parent slice into every child's name. Anchoring on the last
			// "kubepods-" discards any such prefix; requiring it at the front
			// silently reported every pod on such a node as Unknown.
			if start := strings.LastIndex(name, "kubepods-"); start >= 0 {
				name = name[start+len("kubepods-"):]
			}
		}
		switch strings.ToLower(name) {
		case "burstable":
			return QoSBurstable
		case "besteffort":
			return QoSBestEffort
		}
	}

	// Only claim Guaranteed when the path is recognisably a kubepods tree. The
	// containment check matters under a kubelet cgroup root, where no segment
	// starts with "kubepods" and a prefix test reported every Guaranteed pod on
	// such a node as Unknown.
	if inKubepodsTree(segments) {
		return QoSGuaranteed
	}
	return QoSUnknown
}

// splitPath breaks a cgroup path into non-empty segments.
func splitPath(cgroupPath string) []string {
	parts := strings.Split(cgroupPath, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			segments = append(segments, part)
		}
	}
	return segments
}
