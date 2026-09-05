// Package detector observes OOM kills on a node.
//
// Detection sits behind an interface with more than one implementation. The
// polling detector reads counters the kernel already maintains and needs no
// special privileges; the eBPF detector traces the kill itself and reports the
// exact victim. The daemon picks whichever the host supports, so a kernel
// without BTF still produces useful post-mortems.
package detector

import (
	"context"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

// Source identifies which implementation observed an event. It travels with
// every event so a report can state how much to trust the victim details.
type Source string

// Detection sources.
const (
	// SourceEBPF means the kill was traced in the kernel as it happened.
	SourceEBPF Source = "ebpf"
	// SourcePoller means the kill was inferred from cgroup counters afterwards.
	SourcePoller Source = "poller"
	// SourceFake is a scripted event from tests or the e2e suite.
	SourceFake Source = "fake"
)

// Victim describes the process the kernel killed.
type Victim struct {
	// PID is the host-namespace process ID.
	PID int `json:"pid"`
	// NSPid is the PID as seen inside the container.
	NSPid int `json:"nsPid"`
	// Comm is the executable name, as the kernel truncates it to 15 characters.
	Comm string `json:"comm"`
	// Cmdline is the full argument vector when it could be captured.
	Cmdline []string `json:"cmdline,omitempty"`
	// RSSBytes is resident memory at, or shortly before, death.
	RSSBytes uint64 `json:"rssBytes"`
	// Inferred reports how the victim was identified.
	//
	// False means the kernel named this process as it was killed. True means it
	// was deduced afterwards from which processes disappeared, which is the best
	// the polling detector can do and is a guess when several exit at once.
	Inferred bool `json:"inferred"`
	// Known reports whether any victim was identified at all. A kill in a
	// container whose processes were never sampled yields Known false.
	Known bool `json:"known"`
}

// KillEvent is one OOM kill observed on the node.
type KillEvent struct {
	// Time is when the kill was observed, which for the poller is up to one
	// polling interval after it actually happened.
	Time time.Time `json:"time"`
	// CgroupPath is the cgroup whose limit was breached, relative to the
	// hierarchy root.
	CgroupPath string `json:"cgroupPath"`
	// Victim is the killed process.
	Victim Victim `json:"victim"`
	// KillCount is the cgroup's cumulative oom_kill counter after this kill.
	KillCount uint64 `json:"killCount"`
	// GroupKill is memory.oom.group on CgroupPath, read at detection time.
	//
	// Nil means it could not be read, which is a third state and not a false.
	// A detector reads this as early as it can because the flag describes a
	// cgroup that group kill is in the middle of destroying: the one case the
	// flag exists to describe is the one most likely to defeat a later read.
	GroupKill *bool `json:"groupKill,omitempty"`
	// Source is the implementation that observed the event.
	Source Source `json:"source"`
}

// Detector observes OOM kills and publishes them on a channel.
//
// Start must be called once. The returned channel is closed when the context is
// cancelled or Close is called, whichever happens first.
type Detector interface {
	// Start begins detection and returns the event stream.
	Start(ctx context.Context) (<-chan KillEvent, error)
	// Close releases resources. It is safe to call more than once.
	Close() error
	// Source reports which implementation this is.
	Source() Source
}

// victimFromProcess builds an inferred victim from a process snapshot taken
// before the kill.
func victimFromProcess(proc procfs.Process) Victim {
	return Victim{
		PID:      proc.PID,
		NSPid:    proc.NSPid,
		Comm:     proc.Comm,
		Cmdline:  proc.Cmdline,
		RSSBytes: proc.RSSBytes,
		Inferred: true,
		Known:    true,
	}
}
