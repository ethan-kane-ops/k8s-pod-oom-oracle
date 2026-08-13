package detector

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

// wireEvent builds one ring buffer sample by writing fields at explicit byte
// offsets.
//
// It deliberately does not encode a rawEvent: doing so would use the same
// struct definition the decoder uses, and the two errors would cancel out. The
// offsets below are read from struct oom_event in oomtracer.bpf.c, so a change
// to either side that is not mirrored in the other fails here.
func wireEvent(mutate func(b []byte)) []byte {
	b := make([]byte, eventSize)
	binary.LittleEndian.PutUint64(b[0:], 1_500_000_000)  // timestamp_ns
	binary.LittleEndian.PutUint64(b[8:], 4242)           // memcg_id
	binary.LittleEndian.PutUint64(b[16:], 4243)          // task_cgroup_id
	binary.LittleEndian.PutUint64(b[24:], 100)           // anon_rss_pages
	binary.LittleEndian.PutUint64(b[32:], 20)            // file_rss_pages
	binary.LittleEndian.PutUint64(b[40:], 8)             // shmem_rss_pages
	binary.LittleEndian.PutUint64(b[48:], 900)           // total_vm_pages
	binary.LittleEndian.PutUint64(b[56:], 131072)        // limit_pages
	binary.LittleEndian.PutUint64(b[64:], 128)           // badness_points
	binary.LittleEndian.PutUint32(b[72:], 9001)          // pid
	binary.LittleEndian.PutUint32(b[76:], 9002)          // tid
	binary.LittleEndian.PutUint32(b[80:], 1)             // ppid
	binary.LittleEndian.PutUint32(b[84:], 7)             // nspid
	binary.LittleEndian.PutUint32(b[88:], ^uint32(998))  // oom_score_adj, -999 in two's complement
	b[92] = 1                                            // memcg_oom
	copy(b[96:], "tail\x00\x00\x00\x00\x00\x00\x00\x00") // comm
	if mutate != nil {
		mutate(b)
	}
	return b
}

func TestRawEventMatchesWireSize(t *testing.T) {
	t.Parallel()

	// A mismatch here means the probe and the decoder disagree about the
	// layout, which decodes every field at the wrong offset rather than
	// failing.
	if got := binary.Size(rawEvent{}); got != eventSize {
		t.Fatalf("binary.Size(rawEvent{}) = %d, want %d", got, eventSize)
	}
}

func TestDecodeEvent(t *testing.T) {
	t.Parallel()

	event, err := decodeEvent(wireEvent(nil))
	if err != nil {
		t.Fatalf("decodeEvent() error = %v", err)
	}

	tests := []struct {
		field string
		got   any
		want  any
	}{
		{"TimestampNS", event.TimestampNS, uint64(1_500_000_000)},
		{"MemcgID", event.MemcgID, uint64(4242)},
		{"TaskCgroupID", event.TaskCgroupID, uint64(4243)},
		{"AnonRSSPages", event.AnonRSSPages, uint64(100)},
		{"FileRSSPages", event.FileRSSPages, uint64(20)},
		{"ShmemRSSPages", event.ShmemRSSPages, uint64(8)},
		{"TotalVMPages", event.TotalVMPages, uint64(900)},
		{"LimitPages", event.LimitPages, uint64(131072)},
		{"BadnessPoints", event.BadnessPoints, int64(128)},
		{"PID", event.PID, uint32(9001)},
		{"TID", event.TID, uint32(9002)},
		{"PPID", event.PPID, uint32(1)},
		{"NSPid", event.NSPid, uint32(7)},
		{"OOMScoreAdj", event.OOMScoreAdj, int32(-999)},
		{"MemcgOOM", event.MemcgOOM, uint8(1)},
		{"comm()", event.comm(), "tail"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.field, tc.got, tc.want)
		}
	}
}

func TestDecodeEventRejectsWrongSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sample []byte
	}{
		{"empty", nil},
		{"short", make([]byte, eventSize-1)},
		{"long", make([]byte, eventSize+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := decodeEvent(tc.sample); err == nil {
				t.Fatal("decodeEvent() succeeded, want an error")
			}
		})
	}
}

func TestRawEventComm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		comm [16]byte
		want string
	}{
		{
			name: "nul terminated",
			comm: [16]byte{'t', 'a', 'i', 'l'},
			want: "tail",
		},
		{
			// The kernel truncates comm to 15 characters plus a NUL, but a
			// decoder that assumed one would run off the end of a full buffer.
			name: "fills the buffer",
			comm: [16]byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p'},
			want: "abcdefghijklmnop",
		},
		{
			name: "empty",
			comm: [16]byte{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := (rawEvent{Comm: tc.comm}).comm(); got != tc.want {
				t.Errorf("comm() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRawEventKillEvent(t *testing.T) {
	t.Parallel()

	boot := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	raw, err := decodeEvent(wireEvent(nil))
	if err != nil {
		t.Fatalf("decodeEvent() error = %v", err)
	}

	event := raw.killEvent("/kubepods.slice/pod-abc/container", 4096, boot)

	if want := boot.Add(1500 * time.Millisecond); !event.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", event.Time, want)
	}
	if event.Source != SourceEBPF {
		t.Errorf("Source = %q, want %q", event.Source, SourceEBPF)
	}
	if event.CgroupPath != "/kubepods.slice/pod-abc/container" {
		t.Errorf("CgroupPath = %q", event.CgroupPath)
	}
	if event.Victim.PID != 9001 {
		t.Errorf("Victim.PID = %d, want 9001", event.Victim.PID)
	}
	if event.Victim.NSPid != 7 {
		t.Errorf("Victim.NSPid = %d, want 7", event.Victim.NSPid)
	}
	if event.Victim.Comm != "tail" {
		t.Errorf("Victim.Comm = %q, want %q", event.Victim.Comm, "tail")
	}
	// (100 anon + 20 file + 8 shmem) pages at 4 KiB.
	if want := uint64(128 * 4096); event.Victim.RSSBytes != want {
		t.Errorf("Victim.RSSBytes = %d, want %d", event.Victim.RSSBytes, want)
	}
	// The kernel named this process. Nothing about it was deduced, which is
	// the entire difference between this detector and the poller.
	if event.Victim.Inferred {
		t.Error("Victim.Inferred = true, want false for a traced kill")
	}
	if !event.Victim.Known {
		t.Error("Victim.Known = false, want true")
	}
}

func TestRawEventRSSHonoursPageSize(t *testing.T) {
	t.Parallel()

	// arm64 kernels are built with 4K, 16K, or 64K pages. Treating the
	// kernel's page counts as 4 KiB regardless would understate a victim on a
	// 64K-page node by a factor of sixteen.
	raw := rawEvent{AnonRSSPages: 10, FileRSSPages: 5, ShmemRSSPages: 1}

	if got, want := raw.rssBytes(4096), uint64(16*4096); got != want {
		t.Errorf("rssBytes(4096) = %d, want %d", got, want)
	}
	if got, want := raw.rssBytes(65536), uint64(16*65536); got != want {
		t.Errorf("rssBytes(65536) = %d, want %d", got, want)
	}
}

func TestCgroupIndexResolves(t *testing.T) {
	t.Parallel()

	calls := 0
	index := newCgroupIndex(func() (map[uint64]string, error) {
		calls++
		return map[uint64]string{
			100: "/",
			200: "/kubepods.slice",
		}, nil
	})

	path, ok := index.Path(200)
	if !ok || path != "/kubepods.slice" {
		t.Fatalf("Path(200) = %q, %v; want %q, true", path, ok, "/kubepods.slice")
	}
	if calls != 1 {
		t.Fatalf("lister called %d times on first lookup, want 1", calls)
	}

	// A second lookup of a known ID must be served from the cache: the walk is
	// expensive enough that doing it per event would matter during a kill
	// storm, which is exactly when the daemon is busiest.
	if _, ok := index.Path(100); !ok {
		t.Fatal("Path(100) failed on a cached index")
	}
	if calls != 1 {
		t.Errorf("lister called %d times, want the cache to serve the second lookup", calls)
	}
}

func TestCgroupIndexRefreshesOnMiss(t *testing.T) {
	t.Parallel()

	// A container that started since the last walk is not in the index yet,
	// which is the common case: the kill often happens seconds after start.
	generation := 0
	index := newCgroupIndex(func() (map[uint64]string, error) {
		generation++
		if generation == 1 {
			return map[uint64]string{100: "/"}, nil
		}
		return map[uint64]string{100: "/", 300: "/kubepods.slice/new"}, nil
	})

	if _, ok := index.Path(300); ok {
		t.Fatal("Path(300) resolved before the cgroup existed")
	}
	path, ok := index.Path(300)
	if !ok || path != "/kubepods.slice/new" {
		t.Fatalf("Path(300) = %q, %v after refresh; want %q, true", path, ok, "/kubepods.slice/new")
	}
}

func TestCgroupIndexHandlesFailures(t *testing.T) {
	t.Parallel()

	t.Run("zero id is never resolved", func(t *testing.T) {
		t.Parallel()

		// A global OOM leaves memcg_id zero. Walking the hierarchy to look for
		// inode zero would be pure waste.
		index := newCgroupIndex(func() (map[uint64]string, error) {
			t.Error("lister called for a zero id")
			return nil, nil
		})
		if _, ok := index.Path(0); ok {
			t.Error("Path(0) resolved, want false")
		}
	})

	t.Run("lister error is not fatal", func(t *testing.T) {
		t.Parallel()

		index := newCgroupIndex(func() (map[uint64]string, error) {
			return nil, errors.New("cgroupfs unreadable")
		})
		if _, ok := index.Path(42); ok {
			t.Error("Path(42) resolved despite a failing lister")
		}
	})
}
