package cgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// ErrPSIUnsupported is returned when pressure stall information is unavailable.
// Per-cgroup PSI requires the unified hierarchy and a kernel built with
// CONFIG_PSI, so a v1 host or an older kernel legitimately has none.
var ErrPSIUnsupported = errors.New("pressure stall information unavailable")

// ReadPSI reads memory.pressure for a cgroup path relative to the hierarchy
// root. It returns ErrPSIUnsupported when the kernel does not expose it.
func (f *FS) ReadPSI(cgroupPath string) (PSI, error) {
	if f.version != V2 {
		return PSI{}, ErrPSIUnsupported
	}

	name := path.Join(cleanCgroupPath(cgroupPath), "memory.pressure")
	data, err := fs.ReadFile(f.root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return PSI{}, ErrPSIUnsupported
	}
	if err != nil {
		return PSI{}, fmt.Errorf("reading %s: %w", name, err)
	}

	psi, err := ParsePSI(data)
	if err != nil {
		return PSI{}, fmt.Errorf("parsing %s: %w", name, err)
	}
	return psi, nil
}

// ReadPIDs lists the process IDs currently attached to a cgroup.
//
// The list is inherently racy: processes join and leave between the read and
// the caller acting on it. Callers must tolerate PIDs that have already exited.
func (f *FS) ReadPIDs(cgroupPath string) ([]int, error) {
	dir := cleanCgroupPath(cgroupPath)
	if f.version == V1 {
		dir = path.Join("memory", dir)
	}

	name := path.Join(dir, "cgroup.procs")
	data, err := fs.ReadFile(f.root, name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	lines := strings.Fields(string(data))
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		pid, err := strconv.Atoi(line)
		if err != nil {
			return nil, fmt.Errorf("parsing pid %q from %s: %w", line, name, err)
		}
		pids = append(pids, pid)
	}

	return pids, nil
}

// Discover walks the hierarchy beneath prefix and returns every cgroup that
// exposes a memory controller, as paths relative to the hierarchy root.
//
// Unreadable subtrees are skipped rather than aborting the walk. On a live node
// cgroups are created and destroyed continuously, so a directory vanishing
// mid-walk is normal operation and not an error worth surfacing.
func (f *FS) Discover(prefix string) ([]string, error) {
	root := cleanCgroupPath(prefix)
	if f.version == V1 {
		root = path.Join("memory", root)
	}

	marker := "memory.current"
	if f.version == V1 {
		marker = "memory.usage_in_bytes"
	}

	// A missing walk root is a misconfigured prefix, not churn. Surface it here
	// so it cannot masquerade as "no cgroups found" below.
	if _, err := fs.Stat(f.root, root); err != nil {
		return nil, fmt.Errorf("opening cgroup prefix %s: %w", root, err)
	}

	var found []string
	err := fs.WalkDir(f.root, root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that disappeared mid-walk is expected churn.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if errors.Is(err, fs.ErrPermission) {
				return fs.SkipDir
			}
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if _, statErr := fs.Stat(f.root, path.Join(name, marker)); statErr != nil {
			return nil
		}
		found = append(found, f.relativeToHierarchy(name))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking cgroup tree at %s: %w", root, err)
	}

	return found, nil
}

// relativeToHierarchy strips the v1 memory/ prefix so callers always work in
// hierarchy-relative terms regardless of version.
func (f *FS) relativeToHierarchy(name string) string {
	if name == "." {
		return "/"
	}
	if f.version == V1 {
		name = strings.TrimPrefix(strings.TrimPrefix(name, "memory"), "/")
		if name == "" {
			return "/"
		}
	}
	return "/" + name
}
