package cgroup

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

// v2Fixture builds a unified-hierarchy tree holding one container cgroup under
// a realistic kubepods path.
func v2Fixture() fstest.MapFS {
	return fstest.MapFS{
		"cgroup.controllers": &fstest.MapFile{Data: []byte("cpuset cpu io memory pids\n")},
		"memory.current":     &fstest.MapFile{Data: []byte("104857600\n")},
		"memory.max":         &fstest.MapFile{Data: []byte("max\n")},

		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.current": &fstest.MapFile{Data: []byte("536870912\n")},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.max":     &fstest.MapFile{Data: []byte("1073741824\n")},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.peak":    &fstest.MapFile{Data: []byte("805306368\n")},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.swap.current": &fstest.MapFile{
			Data: []byte("4096\n"),
		},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.stat": &fstest.MapFile{
			Data: []byte("anon 402653184\nfile 134217728\nkernel 8388608\nslab 4194304\n"),
		},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.events": &fstest.MapFile{
			Data: []byte("low 0\nhigh 42\nmax 7\noom 3\noom_kill 2\n"),
		},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/memory.pressure": &fstest.MapFile{
			Data: []byte("some avg10=12.00 avg60=6.00 avg300=1.00 total=98765\nfull avg10=4.00 avg60=2.00 avg300=0.50 total=4321\n"),
		},
		"kubepods.slice/kubepods-burstable.slice/pod-abc/container/cgroup.procs": &fstest.MapFile{
			Data: []byte("1234\n1235\n1290\n"),
		},

		// A sparse sibling: only the mandatory files, as an older kernel exposes.
		"kubepods.slice/sparse/memory.current": &fstest.MapFile{Data: []byte("1024\n")},
		"kubepods.slice/sparse/memory.max":     &fstest.MapFile{Data: []byte("2048\n")},

		// A directory with no memory controller at all, which Discover must skip.
		"kubepods.slice/nocontroller/cpu.stat": &fstest.MapFile{Data: []byte("usage_usec 1\n")},
	}
}

// v1Fixture builds a legacy split-hierarchy tree.
func v1Fixture() fstest.MapFS {
	return fstest.MapFS{
		"memory/memory.usage_in_bytes": &fstest.MapFile{Data: []byte("104857600\n")},
		"memory/memory.limit_in_bytes": &fstest.MapFile{Data: []byte("9223372036854771712\n")},

		"memory/kubepods/burstable/pod-abc/container/memory.usage_in_bytes": &fstest.MapFile{
			Data: []byte("536870912\n"),
		},
		"memory/kubepods/burstable/pod-abc/container/memory.limit_in_bytes": &fstest.MapFile{
			Data: []byte("1073741824\n"),
		},
		"memory/kubepods/burstable/pod-abc/container/memory.max_usage_in_bytes": &fstest.MapFile{
			Data: []byte("805306368\n"),
		},
		"memory/kubepods/burstable/pod-abc/container/memory.stat": &fstest.MapFile{
			Data: []byte("cache 134217728\nrss 402653184\nswap 4096\nkernel_stack 8388608\n"),
		},
		"memory/kubepods/burstable/pod-abc/container/memory.failcnt": &fstest.MapFile{Data: []byte("17\n")},
		"memory/kubepods/burstable/pod-abc/container/cgroup.procs":   &fstest.MapFile{Data: []byte("1234\n1235\n")},
	}
}

func TestDetectVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tree fstest.MapFS
		want Version
	}{
		{name: "unified hierarchy", tree: v2Fixture(), want: V2},
		{name: "legacy split hierarchy", tree: v1Fixture(), want: V1},
		{
			name: "legacy hierarchy mounted directly at the memory controller",
			tree: fstest.MapFS{"memory.usage_in_bytes": &fstest.MapFile{Data: []byte("0\n")}},
			want: V1,
		},
		{
			name: "no memory controller",
			tree: fstest.MapFS{"cpu.stat": &fstest.MapFile{Data: []byte("usage_usec 1\n")}},
			want: VersionUnknown,
		},
		{name: "empty tree", tree: fstest.MapFS{}, want: VersionUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DetectVersion(tt.tree); got != tt.want {
				t.Errorf("DetectVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewRejectsHierarchyWithoutMemoryController(t *testing.T) {
	t.Parallel()

	_, err := New(fstest.MapFS{"cpu.stat": &fstest.MapFile{Data: []byte("usage_usec 1\n")}})
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("New() error = %v, want ErrUnsupportedVersion", err)
	}
}

func TestReadMemoryStatsV2(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if f.Version() != V2 {
		t.Fatalf("Version() = %v, want V2", f.Version())
	}

	got, err := f.ReadMemoryStats("/kubepods.slice/kubepods-burstable.slice/pod-abc/container")
	if err != nil {
		t.Fatalf("ReadMemoryStats() error = %v", err)
	}

	want := MemoryStats{
		Current: 536870912,
		Peak:    805306368,
		Limit:   1073741824,
		Swap:    4096,
		Anon:    402653184,
		File:    134217728,
		Kernel:  8388608,
		Events:  MemoryEvents{Low: 0, High: 42, Max: 7, OOM: 3, OOMKill: 2},
	}
	if got != want {
		t.Errorf("ReadMemoryStats() = %+v, want %+v", got, want)
	}
}

func TestReadOOMGroup(t *testing.T) {
	t.Parallel()

	const container = "/kubepods.slice/kubepods-burstable.slice/pod-abc/container"

	tests := []struct {
		name   string
		root   func() fstest.MapFS
		path   string
		want   bool
		wantOK bool
	}{
		{
			// containerd sets this on the container scope, so the whole
			// container dies rather than the one process the kernel picked.
			name: "a group-killed cgroup reports true",
			root: func() fstest.MapFS {
				fsys := v2Fixture()
				fsys[container[1:]+"/memory.oom.group"] = &fstest.MapFile{Data: []byte("1\n")}
				return fsys
			},
			path:   container,
			want:   true,
			wantOK: true,
		},
		{
			name: "an explicit zero reports false",
			root: func() fstest.MapFS {
				fsys := v2Fixture()
				fsys[container[1:]+"/memory.oom.group"] = &fstest.MapFile{Data: []byte("0\n")}
				return fsys
			},
			path:   container,
			want:   false,
			wantOK: true,
		},
		{
			// The file arrived in 4.19. An older kernel simply does not
			// group-kill, so its absence is an answer rather than an error.
			name:   "an absent file reports false without erroring",
			root:   v2Fixture,
			path:   container,
			want:   false,
			wantOK: true,
		},
		{
			// v1 has no equivalent and never group-kills.
			name:   "a v1 hierarchy reports false",
			root:   v1Fixture,
			path:   "/kubepods/burstable/pod-abc/container",
			want:   false,
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := New(tt.root())
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			got, err := f.ReadOOMGroup(tt.path)
			if (err == nil) != tt.wantOK {
				t.Fatalf("ReadOOMGroup() error = %v, wantOK %v", err, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("ReadOOMGroup() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadMemoryStatsV2ToleratesMissingOptionalFiles(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := f.ReadMemoryStats("/kubepods.slice/sparse")
	if err != nil {
		t.Fatalf("ReadMemoryStats() on a sparse cgroup error = %v", err)
	}

	want := MemoryStats{Current: 1024, Limit: 2048}
	if got != want {
		t.Errorf("ReadMemoryStats() = %+v, want %+v", got, want)
	}
}

func TestReadMemoryStatsV1(t *testing.T) {
	t.Parallel()

	f, err := New(v1Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if f.Version() != V1 {
		t.Fatalf("Version() = %v, want V1", f.Version())
	}

	got, err := f.ReadMemoryStats("/kubepods/burstable/pod-abc/container")
	if err != nil {
		t.Fatalf("ReadMemoryStats() error = %v", err)
	}

	want := MemoryStats{
		Current: 536870912,
		Peak:    805306368,
		Limit:   1073741824,
		Swap:    4096,
		Anon:    402653184,
		File:    134217728,
		Kernel:  8388608,
		// v1 has no oom_kill counter; failcnt lands on OOM only.
		Events: MemoryEvents{OOM: 17},
	}
	if got != want {
		t.Errorf("ReadMemoryStats() = %+v, want %+v", got, want)
	}
	if got.Events.OOMKill != 0 {
		t.Error("v1 must never report OOMKill; it cannot distinguish kills from allocation failures")
	}
}

func TestReadMemoryStatsRootCgroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tree      fstest.MapFS
		path      string
		wantLimit uint64
	}{
		{name: "v2 empty path", tree: v2Fixture(), path: "", wantLimit: Unlimited},
		{name: "v2 slash path", tree: v2Fixture(), path: "/", wantLimit: Unlimited},
		{name: "v1 root reports page counter max as unlimited", tree: v1Fixture(), path: "/", wantLimit: Unlimited},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := New(tt.tree)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := f.ReadMemoryStats(tt.path)
			if err != nil {
				t.Fatalf("ReadMemoryStats(%q) error = %v", tt.path, err)
			}
			if got.Limit != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", got.Limit, tt.wantLimit)
			}
			if got.Current != 104857600 {
				t.Errorf("Current = %d, want 104857600", got.Current)
			}
		})
	}
}

func TestReadMemoryStatsMissingCgroup(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	_, err = f.ReadMemoryStats("/kubepods.slice/does-not-exist")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadMemoryStats() error = %v, want it to wrap fs.ErrNotExist", err)
	}
}

func TestCleanCgroupPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: "."},
		{input: "/", want: "."},
		{input: ".", want: "."},
		{input: "/kubepods.slice", want: "kubepods.slice"},
		{input: "kubepods.slice", want: "kubepods.slice"},
		{input: "/kubepods.slice/", want: "kubepods.slice"},
		{input: "//kubepods.slice//pod-abc//", want: "kubepods.slice/pod-abc"},
		{input: "/kubepods.slice/../etc", want: "etc"},
		{input: "../../etc/passwd", want: "etc/passwd"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			if got := cleanCgroupPath(tt.input); got != tt.want {
				t.Errorf("cleanCgroupPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUsageRatio(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stats MemoryStats
		want  float64
	}{
		{name: "half used", stats: MemoryStats{Current: 512, Limit: 1024}, want: 0.5},
		{name: "at the limit", stats: MemoryStats{Current: 1024, Limit: 1024}, want: 1},
		{name: "over the limit clamps to one", stats: MemoryStats{Current: 2048, Limit: 1024}, want: 1},
		{name: "uncapped has no ratio", stats: MemoryStats{Current: 512, Limit: Unlimited}, want: 0},
		{name: "zero limit has no ratio", stats: MemoryStats{Current: 512, Limit: 0}, want: 0},
		{name: "idle", stats: MemoryStats{Current: 0, Limit: 1024}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.stats.UsageRatio(); got != tt.want {
				t.Errorf("UsageRatio() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeadroom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		stats MemoryStats
		want  uint64
	}{
		{name: "room remaining", stats: MemoryStats{Current: 512, Limit: 1024}, want: 512},
		{name: "at the limit", stats: MemoryStats{Current: 1024, Limit: 1024}, want: 0},
		{name: "over the limit never underflows", stats: MemoryStats{Current: 2048, Limit: 1024}, want: 0},
		{name: "uncapped", stats: MemoryStats{Current: 512, Limit: Unlimited}, want: Unlimited},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.stats.Headroom(); got != tt.want {
				t.Errorf("Headroom() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	t.Parallel()

	tests := map[Version]string{V1: "v1", V2: "v2", VersionUnknown: "unknown", Version(99): "unknown"}
	for version, want := range tests {
		if got := version.String(); got != want {
			t.Errorf("Version(%d).String() = %q, want %q", version, got, want)
		}
	}
}
