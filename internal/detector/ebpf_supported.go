//go:build linux && (amd64 || arm64)

package detector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector/bpf"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
)

// ebpfDetector reports OOM kills traced in the kernel.
//
// Unlike the poller it does not deduce anything: the kernel hands over the
// victim it selected, at the moment it selected it, so every field is fact. The
// cost is a kernel that supports BTF and CO-RE and a process privileged enough
// to load a program.
type ebpfDetector struct {
	tracer   *bpf.Tracer
	cgroup   *cgroup.FS
	proc     *procfs.FS
	index    *cgroupIndex
	pageSize int
	bootTime time.Time
	log      *slog.Logger

	events chan KillEvent
	// done is closed by Close and releases the goroutine watching the context.
	done      chan struct{}
	closeOnce sync.Once
}

var _ Detector = (*ebpfDetector)(nil)

// NewEBPF loads and attaches the kernel probe.
//
// It fails when the kernel lacks BTF, when oom_kill_process is not traceable,
// or when the process cannot load BPF programs. Every one of those is a reason
// to fall back to the poller rather than to stop.
func NewEBPF(opts EBPFOptions) (Detector, error) {
	if opts.CgroupRoot == "" {
		return nil, errors.New("ebpf detector requires a cgroup root")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	tracer, err := bpf.Load()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEBPFUnsupported, err)
	}

	// The Go and C sides agree on the layout only if they agree on the size.
	// Checking once at load time turns a silent misdecode into a startup error.
	if bpf.EventSize != eventSize {
		_ = tracer.Close()
		return nil, fmt.Errorf("%w: probe emits %d byte events, decoder expects %d",
			ErrEBPFUnsupported, bpf.EventSize, eventSize)
	}

	bootTime, err := monotonicOrigin()
	if err != nil {
		_ = tracer.Close()
		return nil, fmt.Errorf("reading monotonic clock: %w", err)
	}

	detector := &ebpfDetector{
		tracer:   tracer,
		cgroup:   opts.Cgroup,
		proc:     opts.Proc,
		index:    newCgroupIndex(cgroupInodeLister(opts.CgroupRoot)),
		pageSize: os.Getpagesize(),
		bootTime: bootTime,
		log:      opts.Logger,
		events:   make(chan KillEvent, defaultEventBuffer),
		done:     make(chan struct{}),
	}

	return detector, nil
}

// Source satisfies Detector.
func (d *ebpfDetector) Source() Source { return SourceEBPF }

// Start begins draining the ring buffer.
func (d *ebpfDetector) Start(ctx context.Context) (<-chan KillEvent, error) {
	go func() {
		select {
		case <-ctx.Done():
			// Closing the tracer unblocks the read loop, which is otherwise
			// parked in the kernel and deaf to cancellation.
			_ = d.Close()
		case <-d.done:
		}
	}()

	go d.loop(ctx)

	return d.events, nil
}

// Events returns the event stream.
func (d *ebpfDetector) Events() <-chan KillEvent { return d.events }

// Close detaches the probe. Safe to call more than once.
func (d *ebpfDetector) Close() error {
	var err error
	d.closeOnce.Do(func() {
		close(d.done)
		err = d.tracer.Close()
	})
	return err
}

func (d *ebpfDetector) loop(ctx context.Context) {
	defer close(d.events)

	for {
		sample, err := d.tracer.Read()
		if err != nil {
			// A closed tracer is how this loop is meant to end.
			if !errors.Is(err, bpf.ErrClosed) && ctx.Err() == nil {
				d.log.Error("reading oom ring buffer", "error", err)
			}
			return
		}

		raw, err := decodeEvent(sample)
		if err != nil {
			// One malformed sample is not a reason to stop tracing.
			d.log.Error("decoding oom event", "error", err)
			continue
		}

		select {
		case <-ctx.Done():
			return
		case d.events <- d.enrich(raw):
		}
	}
}

// enrich turns a raw sample into a full event.
//
// The kprobe fires on entry to oom_kill_process, which is before SIGKILL is
// delivered, so the victim is usually still readable in /proc for the moment it
// takes this to run. That is what makes a full command line available at all;
// the kernel itself only records the 15-character comm. When the read loses the
// race, the event is still complete, just terser.
func (d *ebpfDetector) enrich(raw rawEvent) KillEvent {
	var victim procfs.Process
	var victimRead bool
	if d.proc != nil {
		if proc, err := d.proc.Process(int(raw.PID)); err == nil {
			victim, victimRead = proc, true
		}
	}

	event := raw.killEvent(d.resolveCgroup(raw, victim, victimRead), d.pageSize, d.bootTime)

	if victimRead {
		event.Victim.Cmdline = victim.Cmdline
		// The kernel's namespace walk is authoritative, but a zero means the
		// probe could not read it on this kernel.
		if event.Victim.NSPid == 0 {
			event.Victim.NSPid = victim.NSPid
		}
	}

	// The kernel does not report a cumulative count, so it is read back from
	// cgroupfs. The probe fires before the counter is incremented, making this
	// the count as of report time rather than as of the kill.
	if d.cgroup != nil && event.CgroupPath != "" {
		if stats, err := d.cgroup.ReadMemoryStats(event.CgroupPath); err == nil {
			event.KillCount = stats.OOMKills()
		}
	}

	return event
}

// resolveCgroup names the cgroup to attribute the kill to, in descending order
// of trust.
//
// The memcg the kernel recorded is the cgroup whose limit was actually
// breached, which is the same thing the polling detector reports and the only
// answer that is right when a pod-level limit kills a process in a container
// below it. It is absent for a global OOM, where the node ran out of memory and
// no single cgroup is at fault.
func (d *ebpfDetector) resolveCgroup(raw rawEvent, victim procfs.Process, victimRead bool) string {
	if path, ok := d.index.Path(raw.MemcgID); ok {
		return path
	}
	// The victim is still alive at this point often enough to be worth asking.
	if victimRead && victim.CgroupPath != "" {
		return victim.CgroupPath
	}
	if path, ok := d.index.Path(raw.TaskCgroupID); ok {
		return path
	}
	return ""
}

// cgroupInodeLister builds the cgroup ID to path index by walking the
// hierarchy.
//
// A cgroup v2 ID is the inode number of its directory, which is what makes this
// possible at all: there is no kernel interface that maps an ID back to a path.
// Paths are produced in the same form cgroup.FS.Discover returns, since the
// daemon uses them to look up sampled history.
func cgroupInodeLister(root string) cgroupLister {
	return func() (map[uint64]string, error) {
		// An unreadable root is a misconfigured --cgroup-root, not churn.
		// Without this check the walk below would swallow the error and return
		// an empty index, making every traced kill look unattributable.
		if _, err := os.Stat(root); err != nil {
			return nil, fmt.Errorf("opening cgroup root %s: %w", root, err)
		}

		index := make(map[uint64]string)

		err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			// Cgroups are created and destroyed constantly. A directory that
			// vanished mid-walk, or one this process cannot read, is normal and
			// must not abandon the entries already collected.
			if err != nil {
				return nil
			}
			if !entry.IsDir() {
				return nil
			}

			info, err := entry.Info()
			if err != nil {
				return nil
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}

			relative, err := filepath.Rel(root, name)
			if err != nil {
				return nil
			}
			if relative == "." {
				index[stat.Ino] = "/"
				return nil
			}
			index[stat.Ino] = "/" + filepath.ToSlash(relative)

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("indexing cgroups under %s: %w", root, err)
		}

		return index, nil
	}
}

// monotonicOrigin returns the wall clock time the monotonic clock read zero.
//
// The probe timestamps events with bpf_ktime_get_ns, which is CLOCK_MONOTONIC.
// Adding it to this origin converts a kill into a wall clock time a human can
// match against pod logs, which is the only form that is useful in a report.
func monotonicOrigin() (time.Time, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(-time.Duration(ts.Nano())), nil
}
