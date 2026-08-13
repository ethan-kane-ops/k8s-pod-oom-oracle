//go:build linux && (amd64 || arm64)

package detector

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
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
