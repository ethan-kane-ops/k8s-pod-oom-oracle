package detector

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

// ErrEBPFUnsupported is returned when the eBPF detector cannot run: a non-Linux
// build, an architecture with no compiled probe, or a kernel that rejects the
// program. Callers treat it as "fall back to polling", never as a fatal error.
var ErrEBPFUnsupported = errors.New("ebpf detection is unavailable on this host")

// EBPFOptions configures the eBPF detector.
type EBPFOptions struct {
	// CgroupRoot is the path to the cgroup hierarchy, used to turn the cgroup
	// IDs the kernel reports back into paths. Required.
	CgroupRoot string
	// Cgroup reads the kill counter for the affected container. Optional.
	Cgroup *cgroup.FS
	// Proc enriches the traced victim with its full command line. Optional but
	// wanted: the kernel only reports the 15-character comm.
	Proc *procfs.FS
	// Logger receives load failures and malformed samples.
	Logger *slog.Logger
}

// eventSize is the wire size of struct oom_event in oomtracer.bpf.c.
//
// It is asserted against the decoder in the tests. A mismatch between the C
// struct and rawEvent would otherwise decode every field at the wrong offset
// and produce plausible-looking nonsense rather than an error.
const eventSize = 112

// rawEvent mirrors struct oom_event byte for byte.
//
// It is decoded by hand rather than through the type bpf2go generates, so the
// wire format stays readable and testable on any platform. The generated type
// exists only as a cross-check.
type rawEvent struct {
	TimestampNS   uint64
	MemcgID       uint64
	TaskCgroupID  uint64
	AnonRSSPages  uint64
	FileRSSPages  uint64
	ShmemRSSPages uint64
	TotalVMPages  uint64
	LimitPages    uint64
	BadnessPoints int64
	PID           uint32
	TID           uint32
	PPID          uint32
	NSPid         uint32
	OOMScoreAdj   int32
	MemcgOOM      uint8
	_             [3]byte
	Comm          [16]byte
}

// decodeEvent reads one ring buffer sample.
func decodeEvent(sample []byte) (rawEvent, error) {
	if len(sample) != eventSize {
		return rawEvent{}, fmt.Errorf("ebpf sample is %d bytes, want %d", len(sample), eventSize)
	}
	var event rawEvent
	// The kernel writes native byte order, and every architecture with a
	// compiled probe is little-endian.
	if err := binary.Read(bytes.NewReader(sample), binary.LittleEndian, &event); err != nil {
		return rawEvent{}, fmt.Errorf("decoding ebpf sample: %w", err)
	}
	return event, nil
}

// comm returns the victim's executable name, trimmed at the NUL the kernel
// pads it with.
func (r rawEvent) comm() string {
	if end := bytes.IndexByte(r.Comm[:], 0); end >= 0 {
		return string(r.Comm[:end])
	}
	return string(r.Comm[:])
}

// rssBytes totals the victim's resident memory.
//
// The kernel counts pages, and the page size is the running kernel's, not a
// constant: arm64 is built with 4K, 16K, or 64K pages.
func (r rawEvent) rssBytes(pageSize int) uint64 {
	if pageSize <= 0 {
		return 0
	}
	pages := r.AnonRSSPages + r.FileRSSPages + r.ShmemRSSPages
	return pages * uint64(pageSize)
}

// killEvent converts a decoded sample into the detector's public event.
//
// The victim is not marked Inferred: the kernel named this process while
// choosing it, so unlike the poller's guess there is nothing to qualify.
func (r rawEvent) killEvent(cgroupPath string, pageSize int, bootTime time.Time) KillEvent {
	return KillEvent{
		//nolint:gosec // Nanoseconds since boot overflows int64 after 292 years of uptime.
		Time:       bootTime.Add(time.Duration(r.TimestampNS)),
		CgroupPath: cgroupPath,
		Source:     SourceEBPF,
		Victim: Victim{
			PID:      int(r.PID),
			NSPid:    int(r.NSPid),
			Comm:     r.comm(),
			RSSBytes: r.rssBytes(pageSize),
			Inferred: false,
			Known:    true,
		},
	}
}

// cgroupLister reports every cgroup in the hierarchy by inode number.
type cgroupLister func() (map[uint64]string, error)

// cgroupIndex maps the cgroup IDs the kernel reports onto hierarchy paths.
//
// A cgroup v2 ID is the inode number of the cgroup's directory, so the mapping
// is built by walking the hierarchy and stat-ing each directory. The walk is
// not cheap on a busy node, so results are cached and only rebuilt when an ID is
// missing, which happens when a container started since the last rebuild.
type cgroupIndex struct {
	list cgroupLister

	mu   sync.Mutex
	byID map[uint64]string
}

func newCgroupIndex(list cgroupLister) *cgroupIndex {
	return &cgroupIndex{list: list}
}

// Path resolves a cgroup ID, rebuilding the index once on a miss.
func (c *cgroupIndex) Path(id uint64) (string, bool) {
	if id == 0 {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if path, ok := c.byID[id]; ok {
		return path, true
	}

	fresh, err := c.list()
	if err != nil {
		return "", false
	}
	c.byID = fresh

	path, ok := c.byID[id]
	return path, ok
}
