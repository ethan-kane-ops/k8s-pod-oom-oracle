package procfs

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
)

// DefaultRoot is where /proc is mounted on a Linux host. A daemon reading the
// node's processes from inside a pod mounts the host's /proc elsewhere.
const DefaultRoot = "/proc"

// Process is a point-in-time view of one process.
type Process struct {
	// PID is the host-namespace process ID.
	PID int `json:"pid"`
	// NSPid is the PID as seen inside the container. Post-mortems report this
	// alongside the host PID, since it is the one a developer recognises.
	NSPid int `json:"nsPid"`
	// PPID is the host-namespace parent process ID.
	PPID int `json:"ppid"`
	// Comm is the kernel's 15-character executable name.
	Comm string `json:"comm"`
	// Cmdline is the full argument vector, empty for kernel threads.
	Cmdline []string `json:"cmdline"`
	// State is the single-letter scheduler state.
	State string `json:"state"`
	// RSSBytes is resident set size.
	RSSBytes uint64 `json:"rssBytes"`
	// VMSizeBytes is total virtual size.
	VMSizeBytes uint64 `json:"vmSizeBytes"`
	// CgroupPath is the unified (or memory-controller) cgroup path.
	CgroupPath string `json:"cgroupPath"`
	// PIDNamespace is the inode of the process's PID namespace, or zero when
	// the filesystem cannot resolve symlinks.
	PIDNamespace uint64 `json:"pidNamespace"`
}

// FS reads process state from a /proc tree.
type FS struct {
	root fs.FS
}

// New wraps a /proc tree.
func New(root fs.FS) *FS { return &FS{root: root} }

// Default reads the host's /proc.
func Default() *FS { return New(os.DirFS(DefaultRoot)) }

// Process reads a single process. It returns an error wrapping fs.ErrNotExist
// when the process has already exited, which callers must expect: this package
// is used to inspect processes that are in the act of being killed.
func (f *FS) Process(pid int) (Process, error) {
	dir := strconv.Itoa(pid)

	statusData, err := fs.ReadFile(f.root, path.Join(dir, "status"))
	if err != nil {
		return Process{}, fmt.Errorf("reading status for pid %d: %w", pid, err)
	}
	status, err := ParseStatus(statusData)
	if err != nil {
		return Process{}, fmt.Errorf("parsing status for pid %d: %w", pid, err)
	}

	proc := Process{
		PID:         pid,
		NSPid:       status.NSPid,
		PPID:        status.PPID,
		Comm:        status.Name,
		State:       status.State,
		RSSBytes:    status.RSSBytes,
		VMSizeBytes: status.VMSizeBytes,
	}

	// cmdline and cgroup are readable for most processes but not all: kernel
	// threads have no cmdline, and a process can exit between reads. Neither
	// justifies discarding the status we already have.
	if cmdlineData, err := fs.ReadFile(f.root, path.Join(dir, "cmdline")); err == nil {
		proc.Cmdline = ParseCmdline(cmdlineData)
	}
	if cgroupData, err := fs.ReadFile(f.root, path.Join(dir, "cgroup")); err == nil {
		if cgroupPath, err := ParseCgroup(cgroupData); err == nil {
			proc.CgroupPath = cgroupPath
		}
	}
	proc.PIDNamespace = f.namespaceInode(dir)

	return proc, nil
}

// namespaceInode resolves /proc/<pid>/ns/pid, returning zero when the
// filesystem does not support symlinks or the process has exited.
func (f *FS) namespaceInode(dir string) uint64 {
	linkFS, ok := f.root.(fs.ReadLinkFS)
	if !ok {
		return 0
	}
	target, err := linkFS.ReadLink(path.Join(dir, "ns", "pid"))
	if err != nil {
		return 0
	}
	inode, err := ParseNamespaceInode(target)
	if err != nil {
		return 0
	}
	return inode
}

// Processes lists every process currently visible in the tree, sorted by PID.
//
// A process that cannot be read is skipped, whatever the reason. On a live node
// that is the normal case, not an edge case: processes exit between listing the
// directory and reading their status, and a daemon reading the host's /proc
// meets processes it has no permission to inspect. Aborting the scan on the
// first such process would intermittently return nothing at all, which silently
// breaks victim inference rather than degrading it.
//
// Only a failure to list the proc root itself is an error.
func (f *FS) Processes() ([]Process, error) {
	pids, err := f.PIDs()
	if err != nil {
		return nil, err
	}

	procs := make([]Process, 0, len(pids))
	for _, pid := range pids {
		proc, err := f.Process(pid)
		if err != nil {
			continue
		}
		procs = append(procs, proc)
	}
	return procs, nil
}

// ProcessesByCgroup groups every readable process by its cgroup path, each
// group sorted by descending RSS.
//
// Callers watching many cgroups must use this rather than calling
// ProcessesInCgroup per cgroup: /proc is walked once here, instead of once per
// cgroup, which on a busy node is the difference between one scan and hundreds
// per polling pass.
func (f *FS) ProcessesByCgroup() (map[string][]Process, error) {
	all, err := f.Processes()
	if err != nil {
		return nil, err
	}

	grouped := make(map[string][]Process)
	for _, proc := range all {
		if proc.CgroupPath == "" {
			continue
		}
		grouped[proc.CgroupPath] = append(grouped[proc.CgroupPath], proc)
	}
	for path := range grouped {
		sortByMemory(grouped[path])
	}
	return grouped, nil
}

// sortByMemory orders processes heaviest first, PID ascending as a tie-break.
func sortByMemory(procs []Process) {
	slices.SortFunc(procs, func(a, b Process) int {
		if c := cmp.Compare(b.RSSBytes, a.RSSBytes); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
}

// PIDs lists the numeric entries of the tree, sorted ascending.
func (f *FS) PIDs() ([]int, error) {
	entries, err := fs.ReadDir(f.root, ".")
	if err != nil {
		return nil, fmt.Errorf("reading proc root: %w", err)
	}

	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		// Non-numeric entries are the kernel's own files, not processes.
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)

	return pids, nil
}

// ProcessesInCgroup returns the processes whose cgroup path matches exactly,
// sorted by descending RSS so the biggest consumer leads.
//
// This is how the post-mortem builds its list of surviving hog processes after
// a container has lost one of its own.
func (f *FS) ProcessesInCgroup(cgroupPath string) ([]Process, error) {
	all, err := f.Processes()
	if err != nil {
		return nil, err
	}

	matched := make([]Process, 0, len(all))
	for _, proc := range all {
		if proc.CgroupPath == cgroupPath {
			matched = append(matched, proc)
		}
	}
	sortByMemory(matched)

	return matched, nil
}
