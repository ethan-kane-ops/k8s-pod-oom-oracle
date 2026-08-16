// Package render turns a Report into output. Every renderer is a pure function
// of the report, so output is testable with golden files.
package render

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// Display constants for the trajectory chart.
const (
	// barWidth is how many cells the usage bar occupies.
	barWidth = 18
	// maxTrajectoryRows caps how many readings are printed. A full history is
	// 60 samples, which is more noise than signal in a terminal, so it is
	// downsampled evenly with the final reading always kept.
	maxTrajectoryRows = 8
	// maxProcesses caps the process listing.
	maxProcesses = 10
	// oomExitCode is what a shell reports for a SIGKILL from the OOM killer.
	oomExitCode = 137
)

// TextOptions tunes plain-text rendering.
type TextOptions struct {
	// TimeFormat renders timestamps in the trajectory. Defaults to 15:04:05.
	TimeFormat string
	// Location renders timestamps in a zone. Defaults to UTC.
	Location *time.Location
}

func (o TextOptions) withDefaults() TextOptions {
	if o.TimeFormat == "" {
		o.TimeFormat = "15:04:05"
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	return o
}

// Text renders a post-mortem as plain text.
func Text(report *oom.Report, opts TextOptions) string {
	opts = opts.withDefaults()

	var b strings.Builder
	writeHeader(&b, report)
	writeDiagnosis(&b, report, opts)
	writeTrajectory(&b, report, opts)
	writeVictim(&b, report)
	writeProcesses(&b, report)

	return b.String()
}

func writeHeader(b *strings.Builder, report *oom.Report) {
	id := report.Identity
	if id.Resolved {
		fmt.Fprintf(b, "POD: %s (namespace: %s)\n", id.PodName, id.Namespace)
	} else {
		// Without API access the UID is all that is known, and saying so beats
		// printing a blank pod name.
		fmt.Fprintf(b, "POD: <unresolved> (uid: %s)\n", id.PodUID)
	}

	switch {
	case id.ContainerName != "" && id.Image != "":
		fmt.Fprintf(b, "CONTAINER: %s (image: %s)\n", id.ContainerName, id.Image)
	case id.ContainerName != "":
		fmt.Fprintf(b, "CONTAINER: %s\n", id.ContainerName)
	default:
		fmt.Fprintf(b, "CONTAINER: <unresolved> (id: %s)\n", shortID(id.ContainerID))
	}

	if id.QoS != "" {
		fmt.Fprintf(b, "QOS: %s\n", id.QoS)
	}
}

func writeDiagnosis(b *strings.Builder, report *oom.Report, opts TextOptions) {
	fmt.Fprintf(b, "\nDIAGNOSIS: OOMKilled (%s)\n",
		report.Time.In(opts.Location).Format("2006-01-02 15:04:05 MST"))

	if report.LimitBytes > 0 {
		fmt.Fprintf(b, "  Limit:        %s\n", Bytes(report.LimitBytes))
	}
	if report.PeakBytes > 0 {
		fmt.Fprintf(b, "  Peak usage:   %s\n", Bytes(report.PeakBytes))
	}
	if report.KillCount > 0 {
		fmt.Fprintf(b, "  Kill count:   %d\n", report.KillCount)
	}
	fmt.Fprintf(b, "  Detected by:  %s\n", report.Source)

	if report.Trend.Projected {
		fmt.Fprintf(b, "  Growth rate:  %s/s (fit R²=%.2f over %s)\n",
			Bytes(uint64(report.Trend.BytesPerSecond)),
			report.Trend.RSquared,
			report.Trend.Window.Round(time.Second))
	}
}

func writeTrajectory(b *strings.Builder, report *oom.Report, opts TextOptions) {
	if len(report.Trajectory) == 0 {
		b.WriteString("\nMEMORY TRAJECTORY: no samples buffered before the kill\n")
		return
	}

	fmt.Fprintf(b, "\nMEMORY TRAJECTORY (last %s):\n", report.Window().Round(time.Second))

	for _, point := range trajectoryRows(report.Trajectory, maxTrajectoryRows) {
		fmt.Fprintf(b, "  %s: %9s / %-9s %s %3.0f%%",
			point.Time.In(opts.Location).Format(opts.TimeFormat),
			Bytes(point.UsedBytes),
			limitLabel(point.LimitBytes),
			bar(point.Ratio),
			point.Ratio*100)

		// Full pressure means no task in the cgroup progressed at all, which is
		// the clearest pre-kill signal the kernel offers.
		if point.PressureFull > 0 {
			fmt.Fprintf(b, "  (stall %.0f%%)", point.PressureFull)
		}
		b.WriteByte('\n')
	}
}

func writeVictim(b *strings.Builder, report *oom.Report) {
	b.WriteString("\nVICTIM PROCESS:\n")

	if !report.Victim.Known {
		b.WriteString("  Could not be identified.\n")
		b.WriteString("  The kernel reported a kill but no sampled process disappeared.\n")
		return
	}

	victim := report.Victim
	fmt.Fprintf(b, "  PID:             %d (in container: %d)\n", victim.PID, victim.NSPid)
	fmt.Fprintf(b, "  Command:         %s\n", commandLine(victim.Comm, victim.Cmdline))
	fmt.Fprintf(b, "  Exit code:       %d (OOM)\n", oomExitCode)
	if victim.RSSBytes > 0 {
		fmt.Fprintf(b, "  Memory at death: %s\n", Bytes(victim.RSSBytes))
	}

	if victim.Inferred {
		b.WriteString("  Confidence:      inferred, not traced.\n")
		b.WriteString("                   This process vanished between samples and held the\n")
		b.WriteString("                   most memory. Run with the eBPF detector for an exact\n")
		b.WriteString("                   victim.\n")
	} else {
		b.WriteString("  Confidence:      traced in the kernel at the moment of the kill.\n")
	}
}

// writeProcesses lists the container's other processes.
//
// The heading deliberately does not claim survival. Report.GroupKill is false
// both when the container really does survive and when the daemon could not tell,
// so a "SURVIVING PROCESSES" heading would assert something unproven every time
// the cgroup was torn down before it could be read. Stating what was observed,
// and adding the group-kill caveat only when it is known, is true either way.
func writeProcesses(b *strings.Builder, report *oom.Report) {
	if len(report.Processes) == 0 {
		return
	}

	b.WriteString("\nPROCESSES IN CONTAINER AFTER THE KILL:\n")
	if report.GroupKill {
		b.WriteString("  memory.oom.group=1: the kernel killed every process in this\n" +
			"  container, so the list below is a teardown snapshot rather than\n" +
			"  survivors, and resident sizes are already collapsing.\n")
	}

	procs := report.Processes
	truncated := 0
	if len(procs) > maxProcesses {
		truncated = len(procs) - maxProcesses
		procs = procs[:maxProcesses]
	}

	for i, proc := range procs {
		fmt.Fprintf(b, "  %d. %s (PID %d) - %s\n",
			i+1, commandLine(proc.Comm, proc.Cmdline), proc.PID, Bytes(proc.RSSBytes))
	}
	if truncated > 0 {
		fmt.Fprintf(b, "  ... and %d more\n", truncated)
	}
}

// trajectoryRows picks which readings to print.
//
// Even spacing alone is not enough. A container can sit idle for most of its
// history and then balloon in under a second, so evenly spaced rows render a
// flat line for a container that died of memory exhaustion. The busiest reading
// is therefore always retained, replacing whichever selected row sits nearest
// to it in time.
func trajectoryRows(points []oom.TrajectoryPoint, limit int) []oom.TrajectoryPoint {
	if limit <= 0 || len(points) <= limit {
		return points
	}

	peak := 0
	for i := range points {
		if points[i].UsedBytes > points[peak].UsedBytes {
			peak = i
		}
	}

	// Rebuild the even spacing over indices so the peak can be substituted in
	// while keeping the rows in time order.
	indices := make([]int, 0, limit)
	for i := range limit {
		indices = append(indices, i*(len(points)-1)/(limit-1))
	}

	if !slices.Contains(indices, peak) {
		nearest := 0
		for i := range indices {
			if abs(indices[i]-peak) < abs(indices[nearest]-peak) {
				nearest = i
			}
		}
		// Never displace the first or last reading: they anchor the window.
		if nearest == 0 {
			nearest = 1
		}
		if nearest == len(indices)-1 {
			nearest = len(indices) - 2
		}
		indices[nearest] = peak
		slices.Sort(indices)
	}

	rows := make([]oom.TrajectoryPoint, 0, len(indices))
	for _, index := range indices {
		rows = append(rows, points[index])
	}
	return rows
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// downsample reduces a series to at most limit entries, evenly spaced, always
// keeping the first and last.
func downsample[T any](points []T, limit int) []T {
	if limit <= 0 || len(points) <= limit {
		return points
	}

	out := make([]T, 0, limit)
	// Spread indices across the range so the last element lands exactly on the
	// final sample rather than near it.
	for i := range limit {
		index := i * (len(points) - 1) / (limit - 1)
		out = append(out, points[index])
	}
	return out
}

// bar renders a usage ratio as a fixed-width meter.
func bar(ratio float64) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	// Floor rather than round, so a completely full bar means the container is
	// genuinely at its limit. Rounding fills the last cell from about 97%, which
	// on an OOM report reads as "already dead" when it is not.
	filled := int(ratio * barWidth)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled) + "]"
}

// limitLabel renders a memory ceiling, or a dash when uncapped.
func limitLabel(limit uint64) string {
	if limit == 0 {
		return "unlimited"
	}
	return Bytes(limit)
}

// maxCommandLength caps a rendered command so one process cannot dominate the
// report.
const maxCommandLength = 96

// commandLine prefers the full argument vector, falling back to the kernel's
// truncated comm for processes with no cmdline.
//
// Arguments routinely contain newlines and tabs, most obviously an inline
// `sh -c` script, which would otherwise break the report's layout entirely.
// Whitespace is flattened and the result truncated.
func commandLine(comm string, cmdline []string) string {
	if len(cmdline) == 0 {
		return truncate(flattenWhitespace(comm))
	}
	return truncate(flattenWhitespace(strings.Join(cmdline, " ")))
}

// flattenWhitespace collapses every run of whitespace, including newlines and
// control characters, into a single space.
func flattenWhitespace(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}

// truncate shortens a string to maxCommandLength runes, marking the cut.
func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= maxCommandLength {
		return s
	}
	return string(runes[:maxCommandLength-1]) + "…"
}

// shortID truncates a container ID to the 12 characters conventionally shown.
func shortID(id string) string {
	const shortLen = 12
	if len(id) <= shortLen {
		return id
	}
	return id[:shortLen]
}

// Bytes renders a byte count in binary units.
func Bytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	div, exp := uint64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTP"[exp])
}
