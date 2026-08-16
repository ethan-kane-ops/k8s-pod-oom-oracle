// Package oom holds the domain types a post-mortem is built from.
//
// A Report is assembled once, by the daemon, and is then a plain value. Every
// renderer is a pure function of it, which is what makes output testable with
// golden files rather than by scraping a terminal.
package oom

import (
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
)

// TrajectoryPoint is one memory reading on the way to a kill.
type TrajectoryPoint struct {
	// Time is when the reading was taken.
	Time time.Time `json:"time"`
	// UsedBytes is memory in use at that moment.
	UsedBytes uint64 `json:"usedBytes"`
	// LimitBytes is the ceiling in force. Zero when uncapped.
	LimitBytes uint64 `json:"limitBytes"`
	// Ratio is UsedBytes over LimitBytes, in [0,1].
	Ratio float64 `json:"ratio"`
	// PressureFull is the memory.pressure "full" ten-second average, the share
	// of time during which no task in the cgroup made progress.
	PressureFull float64 `json:"pressureFull"`
}

// ProcessSnapshot is one process found in the container's cgroup when the
// report was built, listed by how much memory it holds.
//
// Whether it survived depends on the runtime: see Report.GroupKill.
type ProcessSnapshot struct {
	PID      int      `json:"pid"`
	NSPid    int      `json:"nsPid"`
	Comm     string   `json:"comm"`
	Cmdline  []string `json:"cmdline,omitempty"`
	RSSBytes uint64   `json:"rssBytes"`
}

// Report is a complete post-mortem for one OOM kill.
type Report struct {
	// ID uniquely identifies this report within a daemon's lifetime.
	ID string `json:"id"`
	// Time is when the kill was observed.
	Time time.Time `json:"time"`
	// Identity is the pod and container the kill belongs to.
	Identity correlate.Identity `json:"identity"`
	// Victim is the process the kernel killed.
	Victim detector.Victim `json:"victim"`
	// Source records which detector observed the kill, so a reader knows
	// whether the victim was traced or deduced.
	Source detector.Source `json:"source"`
	// KillCount is the container's cumulative OOM kill count.
	KillCount uint64 `json:"killCount"`
	// LimitBytes is the memory ceiling that was breached.
	LimitBytes uint64 `json:"limitBytes"`
	// PeakBytes is the high-water mark, when the kernel exposes it.
	PeakBytes uint64 `json:"peakBytes"`
	// Trajectory is the buffered memory history leading up to the kill,
	// oldest first. Empty when the container was not sampled for long enough.
	Trajectory []TrajectoryPoint `json:"trajectory"`
	// Processes is the container's process list as it stood when the report was
	// built, heaviest first, with the victim removed.
	//
	// This is a snapshot taken just after the kill, not a survivor list. When
	// GroupKill is false the two are the same thing. When it is true the kernel
	// is killing every process in the cgroup, so this is whatever was still
	// readable mid-teardown: incomplete, and with resident sizes already
	// collapsing towards zero.
	Processes []ProcessSnapshot `json:"processes"`
	// GroupKill records memory.oom.group on the cgroup this report is attributed
	// to. True means the kernel killed the whole cgroup rather than the single
	// process it selected, which is what containerd configures and therefore
	// what almost every cluster does.
	GroupKill bool `json:"groupKill"`
	// Trend is the growth analysis over the trajectory.
	Trend sampler.Trend `json:"trend"`
}

// TrajectoryFrom converts buffered samples into trajectory points.
func TrajectoryFrom(samples []sampler.Sample) []TrajectoryPoint {
	points := make([]TrajectoryPoint, 0, len(samples))
	for i := range samples {
		stats := samples[i].Stats
		limit := stats.Limit
		// An uncapped cgroup reports a sentinel limit that means nothing to a
		// reader, so it is flattened to zero.
		if limit == unlimited {
			limit = 0
		}
		points = append(points, TrajectoryPoint{
			Time:         samples[i].Time,
			UsedBytes:    stats.Current,
			LimitBytes:   limit,
			Ratio:        stats.UsageRatio(),
			PressureFull: samples[i].PSI.Full.Avg10,
		})
	}
	return points
}

// unlimited mirrors cgroup.Unlimited without importing it, keeping this package
// free of a dependency it would otherwise need only for one constant.
const unlimited uint64 = 1<<64 - 1

// ProcessesFrom converts a container's process list into report entries,
// dropping the victim.
//
// Removing the victim is not a plain PID comparison. The eBPF probe reports the
// kernel's global PID, while the listing comes from whichever /proc the daemon
// can see. Those agree on a bare-metal node running with hostPID, and never
// agree under a nested runtime such as kind, where the node is itself a
// container: a victim at global pid 1397320 gets compared against listed pids in
// the ten thousands, so the filter silently never fires and the report names the
// dead process as though it were alive.
//
// NSPid closes the gap. The listing is already scoped to a single cgroup, so a
// container-namespace PID identifies a process within it, and it reads the same
// on both sides however many namespaces sit between the daemon and the kernel.
func ProcessesFrom(procs []procfs.Process, victim detector.Victim) []ProcessSnapshot {
	snapshot := make([]ProcessSnapshot, 0, len(procs))
	nsMatch := victimNSPidIsUnambiguous(procs, victim.NSPid)
	for _, proc := range procs {
		if isVictim(proc, victim, nsMatch) {
			continue
		}
		snapshot = append(snapshot, ProcessSnapshot{
			PID:      proc.PID,
			NSPid:    proc.NSPid,
			Comm:     proc.Comm,
			Cmdline:  proc.Cmdline,
			RSSBytes: proc.RSSBytes,
		})
	}
	return snapshot
}

// isVictim reports whether a listed process is the one the kernel killed.
func isVictim(proc procfs.Process, victim detector.Victim, nsMatch bool) bool {
	// The host PID is authoritative when both sides express it in the same
	// namespace. A match here cannot be a coincidence.
	if victim.PID != 0 && proc.PID == victim.PID {
		return true
	}
	return nsMatch && victim.NSPid != 0 && proc.NSPid == victim.NSPid
}

// victimNSPidIsUnambiguous reports whether exactly one listed process carries
// the victim's container-namespace PID.
//
// A cgroup normally holds one PID namespace, making NSPid unique within it. A
// container running its own nested runtime breaks that, and there the daemon has
// nothing to tell the two apart: the probe reports no namespace inode to compare
// against procfs.Process.PIDNamespace. Leaving the victim in a listing is a
// smaller error than deleting a process that is genuinely there, so an ambiguous
// NSPid disables the fallback rather than guessing.
func victimNSPidIsUnambiguous(procs []procfs.Process, nsPid int) bool {
	if nsPid == 0 {
		return false
	}
	var seen int
	for _, proc := range procs {
		if proc.NSPid == nsPid {
			seen++
		}
	}
	return seen == 1
}

// PeakRatio reports the highest usage ratio seen across the trajectory.
func (r *Report) PeakRatio() float64 {
	var peak float64
	for _, point := range r.Trajectory {
		if point.Ratio > peak {
			peak = point.Ratio
		}
	}
	return peak
}

// Window reports the time span the trajectory covers.
func (r *Report) Window() time.Duration {
	if len(r.Trajectory) < 2 {
		return 0
	}
	return r.Trajectory[len(r.Trajectory)-1].Time.Sub(r.Trajectory[0].Time)
}
