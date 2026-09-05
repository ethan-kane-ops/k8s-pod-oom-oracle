//go:build linux && (amd64 || arm64)

package detector

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

func TestCgroupInodeListerIndexesHierarchy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dirs := []string{
		"kubepods.slice",
		"kubepods.slice/kubepods-burstable.slice",
		"kubepods.slice/kubepods-burstable.slice/cri-containerd-abc.scope",
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	// Files must not be indexed: only a directory is a cgroup.
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("memory\n"), 0o644); err != nil {
		t.Fatalf("writing marker file: %v", err)
	}

	index, err := cgroupInodeLister(root)()
	if err != nil {
		t.Fatalf("cgroupInodeLister() error = %v", err)
	}

	paths := make([]string, 0, len(index))
	for _, path := range index {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	// Paths must match what cgroup.FS.Discover produces, or the daemon cannot
	// join a traced kill to the memory history it sampled for that cgroup.
	want := []string{
		"/",
		"/kubepods.slice",
		"/kubepods.slice/kubepods-burstable.slice",
		"/kubepods.slice/kubepods-burstable.slice/cri-containerd-abc.scope",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("indexed paths = %v, want %v", paths, want)
	}
}

func TestCgroupInodeListerResolvesRealInode(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "some.slice")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("creating cgroup dir: %v", err)
	}

	index, err := cgroupInodeLister(root)()
	if err != nil {
		t.Fatalf("cgroupInodeLister() error = %v", err)
	}

	// The kernel reports a cgroup ID that equals its directory inode, so the
	// index is only useful if it is keyed by the inode the kernel would see.
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("FileInfo.Sys() is %T, want *syscall.Stat_t", info.Sys())
	}
	inode := stat.Ino

	if got := index[inode]; got != "/some.slice" {
		t.Errorf("index[%d] = %q, want %q", inode, got, "/some.slice")
	}
}

func TestCgroupInodeListerMissingRoot(t *testing.T) {
	t.Parallel()

	if _, err := cgroupInodeLister(filepath.Join(t.TempDir(), "absent"))(); err == nil {
		t.Fatal("cgroupInodeLister() succeeded on a missing root, want an error")
	}
}

// TestEnrichReadsGroupKillInTheKillWindow covers the reason the read moved out
// of report assembly.
//
// The kprobe fires on entry to oom_kill_process, before SIGKILL is delivered,
// so the victim's cgroup still exists. Reading memory.oom.group there is the
// only way to get an answer for a group kill at all: by the time a report is
// assembled the runtime has usually destroyed the cgroup holding it, and the
// case the flag exists to describe is the case that erases its own evidence.
//
// enrich is called directly. It touches only the cgroup FS, the proc FS and the
// index, none of which need a loaded probe, so this needs no privileges and no
// BTF.
func TestEnrichReadsGroupKillInTheKillWindow(t *testing.T) {
	t.Parallel()

	const victimCgroup = "/kubepods.slice/pod-a/container"

	tests := []struct {
		name     string
		oomGroup string
		want     *bool
	}{
		{
			// containerd's default.
			name:     "a group-killed cgroup reports true",
			oomGroup: "1\n",
			want:     ptr(true),
		},
		{
			name:     "a cgroup that kills one process reports false",
			oomGroup: "0\n",
			want:     ptr(false),
		},
		{
			// The file arrived in 4.19 and the cgroup is still present, so its
			// absence is an answer rather than a failed read.
			name:     "an absent file inside a live cgroup reports false",
			oomGroup: "",
			want:     ptr(false),
		},
		{
			// Nothing was read, so nothing may be claimed. Reporting false here
			// is how a torn-down container comes to look like a survivor.
			name:     "an absent cgroup reports unknown",
			oomGroup: "missing",
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cgroups := fstest.MapFS{
				"cgroup.controllers": &fstest.MapFile{Data: []byte("memory\n")},
				"memory.current":     &fstest.MapFile{Data: []byte("0\n")},
				"memory.max":         &fstest.MapFile{Data: []byte("max\n")},
			}
			if tt.oomGroup != "missing" {
				key := victimCgroup[1:]
				cgroups[key+"/memory.current"] = &fstest.MapFile{Data: []byte("1048576\n")}
				cgroups[key+"/memory.max"] = &fstest.MapFile{Data: []byte("67108864\n")}
				cgroups[key+"/memory.events"] = &fstest.MapFile{
					Data: []byte("low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\n"),
				}
				if tt.oomGroup != "" {
					cgroups[key+"/memory.oom.group"] = &fstest.MapFile{Data: []byte(tt.oomGroup)}
				}
			}

			cg, err := cgroup.New(cgroups)
			if err != nil {
				t.Fatalf("cgroup.New() error = %v", err)
			}

			procs := fstest.MapFS{
				"meminfo":    &fstest.MapFile{Data: []byte("MemTotal: 16777216 kB\n")},
				"41/status":  &fstest.MapFile{Data: []byte("Name:\tworker\nState:\tR (running)\nPid:\t41\nPPid:\t1\nNSpid:\t41\t7\nVmSize:\t 100000 kB\nVmRSS:\t 4096 kB\n")},
				"41/cmdline": &fstest.MapFile{Data: []byte("worker\x00")},
				"41/cgroup":  &fstest.MapFile{Data: []byte("0::" + victimCgroup + "\n")},
			}

			d := &ebpfDetector{
				cgroup:   cg,
				proc:     procfs.New(procs),
				index:    newCgroupIndex(func() (map[uint64]string, error) { return nil, nil }),
				pageSize: os.Getpagesize(),
				bootTime: epoch,
				log:      slog.New(slog.DiscardHandler),
			}

			raw := rawEvent{PID: 41}
			copy(raw.Comm[:], "worker")

			got := d.enrich(raw)
			if got.CgroupPath != victimCgroup {
				t.Fatalf("CgroupPath = %q, want %q: the victim was read from /proc",
					got.CgroupPath, victimCgroup)
			}
			assertGroupKill(t, got.GroupKill, tt.want)
		})
	}
}
