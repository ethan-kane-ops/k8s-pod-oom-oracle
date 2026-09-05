// Package cgroup reads memory controller state from a cgroup hierarchy.
//
// Every read is routed through an fs.FS so the parsers can be exercised against
// fixture trees on any platform, including machines with no cgroup support.
package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
)

// Version identifies which cgroup hierarchy layout a root uses.
type Version int

const (
	// VersionUnknown means no recognisable memory controller was found.
	VersionUnknown Version = iota
	// V1 is the legacy split-hierarchy layout, memory under a memory/ subtree.
	V1
	// V2 is the unified hierarchy.
	V2
)

// String renders the version as it appears in logs and diagnostics.
func (v Version) String() string {
	switch v {
	case V1:
		return "v1"
	case V2:
		return "v2"
	case VersionUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Unlimited is the sentinel for a cgroup with no memory ceiling. cgroup v2
// spells this "max"; v1 reports a number near the top of the u64 range.
const Unlimited uint64 = 1<<64 - 1

// ErrUnsupportedVersion is returned when a hierarchy exposes no memory controller.
var ErrUnsupportedVersion = errors.New("no cgroup memory controller found")

// ErrNotFound is returned when a requested cgroup path is absent from the
// hierarchy. Callers treat this as "the container is gone", not as a failure.
var ErrNotFound = fs.ErrNotExist

// FS reads memory state from a cgroup hierarchy rooted at an fs.FS.
type FS struct {
	root    fs.FS
	version Version
}

// New wraps a cgroup hierarchy root, detecting its version.
func New(root fs.FS) (*FS, error) {
	version := DetectVersion(root)
	if version == VersionUnknown {
		return nil, ErrUnsupportedVersion
	}
	return &FS{root: root, version: version}, nil
}

// Version reports the detected hierarchy layout.
func (f *FS) Version() Version { return f.version }

// DetectVersion inspects a hierarchy root and reports its layout.
//
// The unified hierarchy is identified by cgroup.controllers at the root. The
// legacy hierarchy is identified by a memory/ subtree carrying the v1-only
// memory.usage_in_bytes file.
func DetectVersion(root fs.FS) Version {
	if _, err := fs.Stat(root, "cgroup.controllers"); err == nil {
		return V2
	}
	if _, err := fs.Stat(root, "memory/memory.usage_in_bytes"); err == nil {
		return V1
	}
	if _, err := fs.Stat(root, "memory.usage_in_bytes"); err == nil {
		return V1
	}
	return VersionUnknown
}

// MemoryEvents mirrors the counters in memory.events. They are monotonic for
// the lifetime of a cgroup, so a rising OOMKill is what signals a fresh kill.
type MemoryEvents struct {
	Low     uint64 `json:"low"`
	High    uint64 `json:"high"`
	Max     uint64 `json:"max"`
	OOM     uint64 `json:"oom"`
	OOMKill uint64 `json:"oomKill"`
}

// MemoryStats is a point-in-time reading of one cgroup's memory controller.
type MemoryStats struct {
	// Current is present usage in bytes.
	Current uint64 `json:"current"`
	// Peak is the high-water mark. Zero when the kernel does not expose it
	// (memory.peak landed in 5.19).
	Peak uint64 `json:"peak"`
	// Limit is the hard ceiling, or Unlimited when uncapped.
	Limit uint64 `json:"limit"`
	// Swap is current swap usage in bytes.
	Swap uint64 `json:"swap"`
	// Anon, File, and Kernel break usage down by type, from memory.stat.
	Anon   uint64 `json:"anon"`
	File   uint64 `json:"file"`
	Kernel uint64 `json:"kernel"`
	// Events holds the memory.events counters. These are hierarchical: a kill in
	// any descendant increments them here too.
	Events MemoryEvents `json:"events"`
	// EventsLocal holds memory.events.local, counting only events charged to
	// this cgroup itself. Attribution must use these, or a kill in one container
	// is reported against every ancestor cgroup as well.
	EventsLocal MemoryEvents `json:"eventsLocal"`
	// HasLocalEvents reports whether memory.events.local was available. It
	// arrived in kernel 5.13; older kernels only expose hierarchical counters.
	HasLocalEvents bool `json:"hasLocalEvents"`
}

// OOMKills reports the kill counter to attribute to this cgroup, preferring the
// local counter so ancestors are not blamed for a descendant's kill.
func (m MemoryStats) OOMKills() uint64 {
	if m.HasLocalEvents {
		return m.EventsLocal.OOMKill
	}
	return m.Events.OOMKill
}

// UsageRatio reports usage as a fraction of the limit, in [0,1]. An uncapped or
// zero limit yields 0, since no ratio is meaningful without a ceiling.
func (m MemoryStats) UsageRatio() float64 {
	if m.Limit == 0 || m.Limit == Unlimited {
		return 0
	}
	ratio := float64(m.Current) / float64(m.Limit)
	if ratio > 1 {
		return 1
	}
	return ratio
}

// Headroom reports bytes remaining before the limit. An uncapped cgroup has
// Unlimited headroom; an over-limit cgroup has none.
func (m MemoryStats) Headroom() uint64 {
	if m.Limit == Unlimited {
		return Unlimited
	}
	if m.Current >= m.Limit {
		return 0
	}
	return m.Limit - m.Current
}

// ReadMemoryStats reads the memory controller for a cgroup path relative to the
// hierarchy root. The empty path reads the root cgroup itself.
func (f *FS) ReadMemoryStats(cgroupPath string) (MemoryStats, error) {
	if f.version == V1 {
		return f.readMemoryStatsV1(cgroupPath)
	}
	return f.readMemoryStatsV2(cgroupPath)
}

// ProcsIn lists the host PIDs currently in a cgroup, from the kernel's own
// membership file.
//
// This exists instead of matching /proc/<pid>/cgroup because that file is
// written relative to the *reading* process's cgroup namespace, and the CRI puts
// an unprivileged pod in a private one. A daemon there reads "0::/" for itself
// and namespace-relative paths for everything else, so comparing them against
// the absolute paths the probe and the sampler use matches nothing and the
// process listing silently comes back empty. Nothing errors; a report simply
// arrives with no processes in it.
//
// cgroup.procs has no such dependency. It lives in the cgroupfs the daemon
// already mounts, it is addressed by the same absolute path everything else
// uses, and the PIDs in it are in the reader's PID namespace, which hostPID
// makes the host's. It is also the kernel's own answer rather than one inferred
// by comparing strings, and reading one file per cgroup is cheaper than walking
// every process on the node.
func (f *FS) ProcsIn(cgroupPath string) ([]int, error) {
	name := path.Join(cleanCgroupPath(cgroupPath), "cgroup.procs")
	data, err := fs.ReadFile(f.root, name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	return ParsePIDList(data)
}

// ReadOOMGroup reports whether the cgroup is killed as an indivisible unit.
//
// When memory.oom.group is set the kernel kills every process in the cgroup
// rather than the single process its badness heuristic picked. containerd sets
// it on the container scope, so on almost every current cluster an OOM takes the
// whole container down. That changes what a process listing taken after the kill
// means, which is why a report carries this alongside the listing itself.
//
// The file is cgroup v2 only, and arrived in 4.19. A v1 hierarchy or an older
// kernel reports false, which is correct: neither group-kills.
//
// A cgroup that no longer exists is an error, not a false. The distinction
// matters more than it looks. Reading the file with readUintOptional alone maps
// both "this kernel predates the file" and "this cgroup was destroyed" onto the
// same false, and the second of those is precisely what group kill does to the
// container being described. Callers would then record "did not group-kill" for
// the case that always group-kills. The directory is therefore stat'd first, so
// an absent file inside a live cgroup stays an answer while an absent cgroup
// becomes an error the caller can report as unknown.
func (f *FS) ReadOOMGroup(cgroupPath string) (bool, error) {
	if f.version == V1 {
		return false, nil
	}
	dir := cleanCgroupPath(cgroupPath)
	if _, err := fs.Stat(f.root, dir); err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	value, err := f.readUintOptional(path.Join(dir, "memory.oom.group"))
	if err != nil {
		return false, err
	}
	return value != 0, nil
}

func (f *FS) readMemoryStatsV2(cgroupPath string) (MemoryStats, error) {
	dir := cleanCgroupPath(cgroupPath)

	current, err := f.readUint(path.Join(dir, "memory.current"))
	if err != nil {
		return MemoryStats{}, err
	}

	limit, err := f.readLimit(path.Join(dir, "memory.max"))
	if err != nil {
		return MemoryStats{}, err
	}

	stats := MemoryStats{Current: current, Limit: limit}

	// memory.peak arrived in 5.19 and memory.swap.current requires the swap
	// controller. An absent file is fine; a corrupt one is not, and must not be
	// silently reported as zero usage.
	if stats.Peak, err = f.readUintOptional(path.Join(dir, "memory.peak")); err != nil {
		return MemoryStats{}, err
	}
	if stats.Swap, err = f.readUintOptional(path.Join(dir, "memory.swap.current")); err != nil {
		return MemoryStats{}, err
	}

	statFields, err := f.readKeyValueOptional(path.Join(dir, "memory.stat"))
	if err != nil {
		return MemoryStats{}, err
	}
	stats.Anon = statFields["anon"]
	stats.File = statFields["file"]
	stats.Kernel = statFields["kernel"]

	eventFields, err := f.readKeyValueOptional(path.Join(dir, "memory.events"))
	if err != nil {
		return MemoryStats{}, err
	}
	stats.Events = eventsFrom(eventFields)

	// memory.events.local landed in 5.13. Where present it is the only correct
	// basis for attribution, since memory.events aggregates descendants.
	localFields, err := f.readKeyValueOptional(path.Join(dir, "memory.events.local"))
	if err != nil {
		return MemoryStats{}, err
	}
	if len(localFields) > 0 {
		stats.EventsLocal = eventsFrom(localFields)
		stats.HasLocalEvents = true
	}

	return stats, nil
}

// eventsFrom maps parsed key/value pairs onto the counter struct.
func eventsFrom(fields map[string]uint64) MemoryEvents {
	return MemoryEvents{
		Low:     fields["low"],
		High:    fields["high"],
		Max:     fields["max"],
		OOM:     fields["oom"],
		OOMKill: fields["oom_kill"],
	}
}

func (f *FS) readMemoryStatsV1(cgroupPath string) (MemoryStats, error) {
	dir := path.Join("memory", cleanCgroupPath(cgroupPath))

	current, err := f.readUint(path.Join(dir, "memory.usage_in_bytes"))
	if err != nil {
		return MemoryStats{}, err
	}

	limit, err := f.readUint(path.Join(dir, "memory.limit_in_bytes"))
	if err != nil {
		return MemoryStats{}, err
	}

	stats := MemoryStats{Current: current, Limit: normaliseLimit(limit)}
	if stats.Peak, err = f.readUintOptional(path.Join(dir, "memory.max_usage_in_bytes")); err != nil {
		return MemoryStats{}, err
	}

	statFields, err := f.readKeyValueOptional(path.Join(dir, "memory.stat"))
	if err != nil {
		return MemoryStats{}, err
	}
	// v1 names differ from v2: rss/cache rather than anon/file.
	stats.Anon = statFields["rss"]
	stats.File = statFields["cache"]
	stats.Kernel = statFields["kernel_stack"]
	stats.Swap = statFields["swap"]

	// v1 has no memory.events. The nearest equivalent to an OOM-kill counter is
	// the failcnt on the memory limit, which counts allocation failures rather
	// than kills. It is exposed as OOM so pressure trending still works, and is
	// deliberately not mapped to OOMKill: only v2 can report actual kills.
	if stats.Events.OOM, err = f.readUintOptional(path.Join(dir, "memory.failcnt")); err != nil {
		return MemoryStats{}, err
	}

	return stats, nil
}

// pageCounterMax is what the kernel reports for an uncapped memory limit on a
// 64-bit host: LONG_MAX rounded down to a page boundary. cgroup v1 writes this
// literal into memory.limit_in_bytes instead of a "max" keyword.
const pageCounterMax uint64 = 0x7FFFFFFFFFFFF000

// normaliseLimit maps a kernel "no limit" sentinel onto Unlimited. Anything at
// or above pageCounterMax is uncapped in practice, since no host has 9 exabytes
// of memory to cap.
func normaliseLimit(limit uint64) uint64 {
	if limit >= pageCounterMax {
		return Unlimited
	}
	return limit
}

// cleanCgroupPath normalises a caller-supplied cgroup path into an fs.FS-safe
// relative path. fs.FS rejects leading slashes and "." is its root.
func cleanCgroupPath(p string) string {
	cleaned := path.Clean("/" + p)
	if cleaned == "/" {
		return "."
	}
	return cleaned[1:]
}

func (f *FS) readUint(name string) (uint64, error) {
	data, err := fs.ReadFile(f.root, name)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	value, err := ParseUint(data)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return value, nil
}

// readUintOptional returns zero for files the running kernel does not expose.
func (f *FS) readUintOptional(name string) (uint64, error) {
	value, err := f.readUint(name)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	return value, err
}

func (f *FS) readLimit(name string) (uint64, error) {
	data, err := fs.ReadFile(f.root, name)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", name, err)
	}
	value, err := ParseLimit(data)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", name, err)
	}
	return value, nil
}

func (f *FS) readKeyValueOptional(name string) (map[string]uint64, error) {
	data, err := fs.ReadFile(f.root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]uint64{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	fields, err := ParseKeyValue(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	return fields, nil
}
