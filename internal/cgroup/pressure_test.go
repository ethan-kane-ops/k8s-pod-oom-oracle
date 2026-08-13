package cgroup

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"
)

const containerPathV2 = "/kubepods.slice/kubepods-burstable.slice/pod-abc/container"

func TestReadPSI(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := f.ReadPSI(containerPathV2)
	if err != nil {
		t.Fatalf("ReadPSI() error = %v", err)
	}

	want := PSI{
		Some: PSILine{Avg10: 12, Avg60: 6, Avg300: 1, Total: 98765},
		Full: PSILine{Avg10: 4, Avg60: 2, Avg300: 0.5, Total: 4321},
	}
	if got != want {
		t.Errorf("ReadPSI() = %+v, want %+v", got, want)
	}
}

func TestReadPSIUnsupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tree fstest.MapFS
		path string
	}{
		{
			name: "v1 hierarchy has no per-cgroup PSI",
			tree: v1Fixture(),
			path: "/kubepods/burstable/pod-abc/container",
		},
		{
			name: "kernel without CONFIG_PSI omits the file",
			tree: v2Fixture(),
			path: "/kubepods.slice/sparse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := New(tt.tree)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := f.ReadPSI(tt.path); !errors.Is(err, ErrPSIUnsupported) {
				t.Errorf("ReadPSI() error = %v, want ErrPSIUnsupported", err)
			}
		})
	}
}

func TestReadPIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tree fstest.MapFS
		path string
		want []int
	}{
		{name: "v2", tree: v2Fixture(), path: containerPathV2, want: []int{1234, 1235, 1290}},
		{name: "v1", tree: v1Fixture(), path: "/kubepods/burstable/pod-abc/container", want: []int{1234, 1235}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := New(tt.tree)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := f.ReadPIDs(tt.path)
			if err != nil {
				t.Fatalf("ReadPIDs() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ReadPIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadPIDsEmptyCgroup(t *testing.T) {
	t.Parallel()

	tree := v2Fixture()
	tree["kubepods.slice/empty/memory.current"] = &fstest.MapFile{Data: []byte("0\n")}
	tree["kubepods.slice/empty/memory.max"] = &fstest.MapFile{Data: []byte("max\n")}
	tree["kubepods.slice/empty/cgroup.procs"] = &fstest.MapFile{Data: []byte("")}

	f, err := New(tree)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := f.ReadPIDs("/kubepods.slice/empty")
	if err != nil {
		t.Fatalf("ReadPIDs() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadPIDs() = %v, want empty", got)
	}
}

func TestReadPIDsMalformed(t *testing.T) {
	t.Parallel()

	tree := v2Fixture()
	tree["kubepods.slice/bad/memory.current"] = &fstest.MapFile{Data: []byte("0\n")}
	tree["kubepods.slice/bad/memory.max"] = &fstest.MapFile{Data: []byte("max\n")}
	tree["kubepods.slice/bad/cgroup.procs"] = &fstest.MapFile{Data: []byte("1234\nnotapid\n")}

	f, err := New(tree)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := f.ReadPIDs("/kubepods.slice/bad"); err == nil {
		t.Fatal("ReadPIDs() = nil error on malformed content, want error")
	}
}

func TestDiscover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tree       fstest.MapFS
		prefix     string
		want       []string
		wantAbsent []string
	}{
		{
			name:   "v2 finds every cgroup carrying a memory controller",
			tree:   v2Fixture(),
			prefix: "/kubepods.slice",
			want: []string{
				"/kubepods.slice/kubepods-burstable.slice/pod-abc/container",
				"/kubepods.slice/sparse",
			},
			wantAbsent: []string{"/kubepods.slice/nocontroller"},
		},
		{
			name:   "v2 from the root includes the root cgroup itself",
			tree:   v2Fixture(),
			prefix: "/",
			want:   []string{"/", "/kubepods.slice/sparse"},
		},
		{
			name:   "v1 paths are reported without the memory prefix",
			tree:   v1Fixture(),
			prefix: "/kubepods",
			want:   []string{"/kubepods/burstable/pod-abc/container"},
			wantAbsent: []string{
				"/memory/kubepods/burstable/pod-abc/container",
				"memory/kubepods/burstable/pod-abc/container",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := New(tt.tree)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			got, err := f.Discover(tt.prefix)
			if err != nil {
				t.Fatalf("Discover(%q) error = %v", tt.prefix, err)
			}

			for _, want := range tt.want {
				if !slices.Contains(got, want) {
					t.Errorf("Discover(%q) = %v, missing %q", tt.prefix, got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if slices.Contains(got, absent) {
					t.Errorf("Discover(%q) = %v, must not contain %q", tt.prefix, got, absent)
				}
			}
		})
	}
}

func TestDiscoverResultsAreReadable(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	paths, err := f.Discover("/kubepods.slice")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("Discover() returned nothing")
	}

	// Every discovered path must feed straight back into ReadMemoryStats.
	for _, p := range paths {
		if _, err := f.ReadMemoryStats(p); err != nil {
			t.Errorf("ReadMemoryStats(%q) error = %v; Discover returned an unusable path", p, err)
		}
	}
}

func TestDiscoverMissingPrefix(t *testing.T) {
	t.Parallel()

	f, err := New(v2Fixture())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := f.Discover("/nope"); err == nil {
		t.Fatal("Discover() on a missing prefix = nil error, want error")
	}
}
