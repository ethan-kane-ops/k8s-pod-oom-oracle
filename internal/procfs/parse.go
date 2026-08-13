// Package procfs reads process state from a /proc filesystem.
//
// As with the cgroup package, every read is routed through an fs.FS so the
// parsers run against fixture trees on any platform.
package procfs

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// kibibyte is the unit /proc/<pid>/status reports memory sizes in.
const kibibyte = 1024

// Status is the subset of /proc/<pid>/status this project needs.
type Status struct {
	// Name is the executable name, truncated by the kernel to 15 characters.
	Name string
	// State is the single-letter scheduler state, such as R, S, or D.
	State string
	// PID and PPID are host-namespace identifiers.
	PID  int
	PPID int
	// NSPid is the PID as seen from the innermost namespace the process is in.
	// For a containerised process this is the PID a developer sees with `ps`
	// inside the container, which is rarely the host PID.
	NSPid int
	// RSSBytes is resident set size. Kernel threads report zero.
	RSSBytes uint64
	// VMSizeBytes is total virtual size.
	VMSizeBytes uint64
}

// ParseStatus reads /proc/<pid>/status.
//
// Unknown fields are ignored: the file grows between kernel releases, and the
// fields this project needs have been stable for a decade.
func ParseStatus(data []byte) (Status, error) {
	var status Status

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)

		var err error
		switch key {
		case "Name":
			status.Name = value
		case "State":
			// "S (sleeping)" reduces to "S".
			status.State, _, _ = strings.Cut(value, " ")
		case "Pid":
			status.PID, err = strconv.Atoi(value)
		case "PPid":
			status.PPID, err = strconv.Atoi(value)
		case "NSpid":
			status.NSPid, err = parseInnermostNSPid(value)
		case "VmRSS":
			status.RSSBytes, err = parseSizeKB(value)
		case "VmSize":
			status.VMSizeBytes, err = parseSizeKB(value)
		}
		if err != nil {
			return Status{}, fmt.Errorf("parsing %s field: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Status{}, fmt.Errorf("scanning status content: %w", err)
	}

	// A process outside any PID namespace has no NSpid line; it is its own
	// innermost view.
	if status.NSPid == 0 {
		status.NSPid = status.PID
	}

	return status, nil
}

// parseInnermostNSPid reads the last entry of an NSpid line. The kernel lists
// one PID per namespace from outermost to innermost, so the final entry is the
// PID as seen inside the container.
func parseInnermostNSPid(value string) (int, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, nil
	}
	pid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 0, fmt.Errorf("parsing namespace pid %q: %w", fields[len(fields)-1], err)
	}
	return pid, nil
}

// parseSizeKB reads a "1234 kB" value and returns bytes.
func parseSizeKB(value string) (uint64, error) {
	digits, _, _ := strings.Cut(value, " ")
	size, err := strconv.ParseUint(strings.TrimSpace(digits), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing size %q: %w", value, err)
	}
	return size * kibibyte, nil
}

// ParseCmdline splits the NUL-separated /proc/<pid>/cmdline.
//
// Kernel threads have an empty cmdline, which yields a nil slice.
func ParseCmdline(data []byte) []string {
	trimmed := bytes.TrimRight(data, "\x00")
	if len(trimmed) == 0 {
		return nil
	}

	parts := bytes.Split(trimmed, []byte{0})
	args := make([]string, 0, len(parts))
	for _, part := range parts {
		args = append(args, string(part))
	}
	return args
}

// ParseCgroup reads the cgroup path from /proc/<pid>/cgroup.
//
// The unified hierarchy writes a single "0::/path" line. The legacy hierarchy
// writes one line per controller as "id:controllers:/path", and the memory
// controller is the one that matters here. Unified wins when both are present,
// as on a hybrid host.
func ParseCgroup(data []byte) (string, error) {
	var unified, memory string

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		// Format is id:controllers:path, and the path itself may contain colons,
		// so split into exactly three parts.
		fields := strings.SplitN(scanner.Text(), ":", 3)
		if len(fields) != 3 {
			continue
		}
		controllers, cgroupPath := fields[1], fields[2]

		if controllers == "" {
			unified = cgroupPath
			continue
		}
		for _, controller := range strings.Split(controllers, ",") {
			if controller == "memory" {
				memory = cgroupPath
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scanning cgroup content: %w", err)
	}

	if unified != "" {
		return unified, nil
	}
	if memory != "" {
		return memory, nil
	}
	return "", fmt.Errorf("no unified or memory cgroup line found")
}

// ParseNamespaceInode extracts the inode from a namespace symlink target of the
// form "pid:[4026531836]". Processes sharing an inode share that namespace.
func ParseNamespaceInode(target string) (uint64, error) {
	open := strings.IndexByte(target, '[')
	closeIdx := strings.IndexByte(target, ']')
	if open < 0 || closeIdx < open {
		return 0, fmt.Errorf("malformed namespace link %q", target)
	}

	inode, err := strconv.ParseUint(target[open+1:closeIdx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing namespace inode from %q: %w", target, err)
	}
	return inode, nil
}
