package procfs

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"
)

const containerCgroup = "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod-abc.slice/cri-containerd-deadbeef.scope"

// procEntry describes one process to write into a fixture tree.
type procEntry struct {
	pid     string
	status  string
	cmdline string
	cgroup  string
	nsInode string
}

// buildTree renders proc entries into an fstest.MapFS, including the kernel's
// own non-numeric files so PID enumeration is exercised realistically.
func buildTree(entries ...procEntry) fstest.MapFS {
	tree := fstest.MapFS{
		"meminfo":     &fstest.MapFile{Data: []byte("MemTotal: 16777216 kB\n")},
		"self":        &fstest.MapFile{Data: []byte(""), Mode: fs.ModeSymlink},
		"sys/kernel":  &fstest.MapFile{Data: []byte("")},
		"uptime":      &fstest.MapFile{Data: []byte("1234.56 987.65\n")},
		"loadavg":     &fstest.MapFile{Data: []byte("0.1 0.2 0.3 1/200 12345\n")},
		"filesystems": &fstest.MapFile{Data: []byte("nodev\tcgroup2\n")},
	}

	for _, e := range entries {
		tree[e.pid+"/status"] = &fstest.MapFile{Data: []byte(e.status)}
		if e.cmdline != "" {
			tree[e.pid+"/cmdline"] = &fstest.MapFile{Data: []byte(e.cmdline)}
		}
		if e.cgroup != "" {
			tree[e.pid+"/cgroup"] = &fstest.MapFile{Data: []byte(e.cgroup)}
		}
		if e.nsInode != "" {
			tree[e.pid+"/ns/pid"] = &fstest.MapFile{
				Data: []byte("pid:[" + e.nsInode + "]"),
				Mode: fs.ModeSymlink,
			}
		}
	}
	return tree
}

// status renders a minimal but realistic /proc/<pid>/status body.
func status(name, pid, ppid, nspid, rssKB string) string {
	return "Name:\t" + name + "\nState:\tS (sleeping)\nPid:\t" + pid +
		"\nPPid:\t" + ppid + "\nNSpid:\t" + pid + "\t" + nspid +
		"\nVmSize:\t 1216304 kB\nVmRSS:\t " + rssKB + " kB\n"
}

func TestProcess(t *testing.T) {
	t.Parallel()

	tree := buildTree(procEntry{
		pid:     "28145",
		status:  status("node", "28145", "28102", "17", "116736"),
		cmdline: "node\x00./dist/garbage-collector.js\x00",
		cgroup:  "0::" + containerCgroup + "\n",
		nsInode: "4026532715",
	})

	got, err := New(tree).Process(28145)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}

	want := Process{
		PID:          28145,
		NSPid:        17,
		PPID:         28102,
		Comm:         "node",
		Cmdline:      []string{"node", "./dist/garbage-collector.js"},
		State:        "S",
		RSSBytes:     116736 * 1024,
		VMSizeBytes:  1216304 * 1024,
		CgroupPath:   containerCgroup,
		PIDNamespace: 4026532715,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Process() = %+v\nwant %+v", got, want)
	}
}

func TestProcessMissing(t *testing.T) {
	t.Parallel()

	_, err := New(buildTree()).Process(999)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Process() error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

// TestProcessPartialReads covers the reads that are allowed to fail. A kernel
// thread has no cmdline, and a process can lose its cgroup file mid-read. The
// status already gathered must survive either.
func TestProcessPartialReads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry procEntry
		check func(t *testing.T, p Process)
	}{
		{
			name:  "kernel thread without a cmdline",
			entry: procEntry{pid: "42", status: status("kworker/0:1", "42", "2", "42", "0")},
			check: func(t *testing.T, p Process) {
				if p.Cmdline != nil {
					t.Errorf("Cmdline = %q, want nil", p.Cmdline)
				}
				if p.Comm != "kworker/0:1" {
					t.Errorf("Comm = %q, want the status still parsed", p.Comm)
				}
			},
		},
		{
			name: "process without a cgroup file",
			entry: procEntry{
				pid: "7", status: status("init", "7", "1", "7", "1024"), cmdline: "init\x00",
			},
			check: func(t *testing.T, p Process) {
				if p.CgroupPath != "" {
					t.Errorf("CgroupPath = %q, want empty", p.CgroupPath)
				}
				if p.Comm != "init" {
					t.Errorf("Comm = %q, want the status still parsed", p.Comm)
				}
			},
		},
		{
			name: "process with an unparseable cgroup file",
			entry: procEntry{
				pid: "8", status: status("app", "8", "1", "8", "2048"), cgroup: "garbage\n",
			},
			check: func(t *testing.T, p Process) {
				if p.CgroupPath != "" {
					t.Errorf("CgroupPath = %q, want empty when the file cannot be parsed", p.CgroupPath)
				}
			},
		},
		{
			name: "process without a namespace link",
			entry: procEntry{
				pid: "9", status: status("app", "9", "1", "9", "2048"), cgroup: "0::/\n",
			},
			check: func(t *testing.T, p Process) {
				if p.PIDNamespace != 0 {
					t.Errorf("PIDNamespace = %d, want 0", p.PIDNamespace)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pid := 0
			for _, c := range tt.entry.pid {
				pid = pid*10 + int(c-'0')
			}

			got, err := New(buildTree(tt.entry)).Process(pid)
			if err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestPIDsIgnoresKernelFiles(t *testing.T) {
	t.Parallel()

	tree := buildTree(
		procEntry{pid: "100", status: status("a", "100", "1", "100", "1024")},
		procEntry{pid: "2", status: status("b", "2", "1", "2", "2048")},
		procEntry{pid: "37", status: status("c", "37", "1", "37", "512")},
	)

	got, err := New(tree).PIDs()
	if err != nil {
		t.Fatalf("PIDs() error = %v", err)
	}
	if want := []int{2, 37, 100}; !reflect.DeepEqual(got, want) {
		t.Errorf("PIDs() = %v, want %v sorted with meminfo and friends excluded", got, want)
	}
}

func TestProcesses(t *testing.T) {
	t.Parallel()

	tree := buildTree(
		procEntry{pid: "1", status: status("init", "1", "0", "1", "1024"), cgroup: "0::/\n"},
		procEntry{pid: "2", status: status("app", "2", "1", "2", "4096"), cgroup: "0::" + containerCgroup + "\n"},
	)

	got, err := New(tree).Processes()
	if err != nil {
		t.Fatalf("Processes() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Processes()) = %d, want 2", len(got))
	}
	if got[0].PID != 1 || got[1].PID != 2 {
		t.Errorf("Processes() not sorted by PID: %d then %d", got[0].PID, got[1].PID)
	}
}

// TestProcessesSkipsRacingExit covers the common case on a node mid-OOM: the
// directory is listed, then the process is reaped before its status is read.
func TestProcessesSkipsRacingExit(t *testing.T) {
	t.Parallel()

	tree := buildTree(procEntry{pid: "1", status: status("init", "1", "0", "1", "1024")})
	// A PID directory with no status file, as a reaped process leaves behind.
	tree["999/cmdline"] = &fstest.MapFile{Data: []byte("gone\x00")}

	got, err := New(tree).Processes()
	if err != nil {
		t.Fatalf("Processes() error = %v; a process exiting mid-scan must not fail the scan", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Processes()) = %d, want only the surviving process", len(got))
	}
	if got[0].PID != 1 {
		t.Errorf("surviving PID = %d, want 1", got[0].PID)
	}
}

func TestProcessesWithPIDsSortsByDescendingRSS(t *testing.T) {
	t.Parallel()

	tree := buildTree(
		// The container's processes, deliberately out of memory order.
		procEntry{pid: "28145", status: status("gc", "28145", "28102", "3", "116736"), cgroup: "0::" + containerCgroup + "\n"},
		procEntry{pid: "28102", status: status("server", "28102", "1", "1", "399360"), cgroup: "0::" + containerCgroup + "\n"},
		procEntry{pid: "28160", status: status("worker", "28160", "28102", "4", "116736"), cgroup: "0::" + containerCgroup + "\n"},
		// A process in a different container, which must not appear.
		procEntry{pid: "500", status: status("other", "500", "1", "1", "999999"), cgroup: "0::/kubepods.slice/other.scope\n"},
		// A host process.
		procEntry{pid: "1", status: status("systemd", "1", "0", "1", "8192"), cgroup: "0::/init.scope\n"},
	)

	// The PIDs come from the cgroup's own cgroup.procs, which is what the
	// callers read: /proc/<pid>/cgroup is written relative to the reader's
	// cgroup namespace and matches nothing from an unprivileged pod.
	got := New(tree).ProcessesWithPIDs([]int{28145, 28102, 28160})

	gotPIDs := make([]int, len(got))
	for i, p := range got {
		gotPIDs[i] = p.PID
	}
	// Biggest consumer first; the two equal-RSS processes tie-break by PID.
	if want := []int{28102, 28145, 28160}; !reflect.DeepEqual(gotPIDs, want) {
		t.Errorf("ProcessesWithPIDs() PIDs = %v, want %v", gotPIDs, want)
	}
}

// TestProcessesWithPIDsSkipsUnreadable covers the normal case rather than an
// edge one. cgroup.procs is a snapshot, so a process listed in it can exit
// before it is read, and that is exactly what happens while a container is
// being torn down: the moment this is most often called.
func TestProcessesWithPIDsSkipsUnreadable(t *testing.T) {
	t.Parallel()

	tree := buildTree(
		procEntry{pid: "28102", status: status("server", "28102", "1", "1", "399360"), cgroup: "0::" + containerCgroup + "\n"},
	)

	got := New(tree).ProcessesWithPIDs([]int{28102, 999999})
	if len(got) != 1 {
		t.Fatalf("ProcessesWithPIDs() returned %d processes, want 1: a pid that has "+
			"exited must be skipped rather than losing the ones still readable", len(got))
	}
	if got[0].PID != 28102 {
		t.Errorf("ProcessesWithPIDs()[0].PID = %d, want 28102", got[0].PID)
	}
}

func TestProcessesWithPIDsNoPIDs(t *testing.T) {
	t.Parallel()

	tree := buildTree(procEntry{pid: "1", status: status("init", "1", "0", "1", "1024"), cgroup: "0::/\n"})

	if got := New(tree).ProcessesWithPIDs(nil); len(got) != 0 {
		t.Errorf("ProcessesWithPIDs(nil) = %v, want empty", got)
	}
}

func TestDefaultReadsHostProc(t *testing.T) {
	t.Parallel()

	// Only asserts wiring; the host may not even have /proc.
	if Default() == nil {
		t.Fatal("Default() = nil")
	}
}
