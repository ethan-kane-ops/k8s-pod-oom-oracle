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

// HogProcess is a process that survived, listed by how much memory it holds.
type HogProcess struct {
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
	// Hogs are the processes still alive in the container after the kill,
	// heaviest first.
	Hogs []HogProcess `json:"hogs"`
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

// HogsFrom converts surviving processes into report entries, dropping the
// victim if it is somehow still listed.
func HogsFrom(procs []procfs.Process, victimPID int) []HogProcess {
	hogs := make([]HogProcess, 0, len(procs))
	for _, proc := range procs {
		if proc.PID == victimPID {
			continue
		}
		hogs = append(hogs, HogProcess{
			PID:      proc.PID,
			NSPid:    proc.NSPid,
			Comm:     proc.Comm,
			Cmdline:  proc.Cmdline,
			RSSBytes: proc.RSSBytes,
		})
	}
	return hogs
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
