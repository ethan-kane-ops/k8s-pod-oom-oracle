//go:build linux && (amd64 || arm64)

package bpf

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// KprobeSymbol is the kernel function the probe attaches to. Exported so a
// failure to attach can name it in the error a user actually sees.
const KprobeSymbol = "oom_kill_process"

// EventSize is the size of one ring buffer sample, in bytes. The decoder in
// internal/detector checks samples against it.
const EventSize = 112

// ErrClosed is returned by Read after Close.
var ErrClosed = ringbuf.ErrClosed

// Tracer is a loaded and attached OOM tracer.
//
// It owns everything kernel-side: the verified program, the kprobe holding it
// on oom_kill_process, and the ring buffer it writes to. Callers get raw
// samples and decode them themselves, which keeps the wire format decodable on
// platforms that cannot load BPF at all.
type Tracer struct {
	objs   oomtracerObjects
	kprobe link.Link
	reader *ringbuf.Reader
}

// Load verifies the probe, attaches it, and opens the ring buffer.
//
// Failure here is expected on kernels without BTF or without the traced symbol,
// and the caller is meant to fall back to polling rather than give up.
func Load() (*Tracer, error) {
	// The kernel charges BPF maps and programs to RLIMIT_MEMLOCK only before
	// 5.11; newer kernels account that memory to the cgroup and ignore the
	// limit entirely. Failing to raise it is therefore not a reason to give up
	// before trying to load, and treating it as one made a container that could
	// have traced fine fall back to polling. The error is kept back in case the
	// load does fail, where it is the likely cause.
	memlockErr := rlimit.RemoveMemlock()

	tracer := &Tracer{}
	if err := loadOomtracerObjects(&tracer.objs, nil); err != nil {
		if memlockErr != nil {
			return nil, fmt.Errorf("loading oom tracer: %w (memlock limit was not raised: %w)",
				verifierDetail(err), memlockErr)
		}
		return nil, fmt.Errorf("loading oom tracer: %w", verifierDetail(err))
	}

	kprobe, err := link.Kprobe(KprobeSymbol, tracer.objs.TraceOomKillProcess, nil)
	if err != nil {
		_ = tracer.objs.Close()
		return nil, fmt.Errorf("attaching kprobe to %s: %w", KprobeSymbol, err)
	}
	tracer.kprobe = kprobe

	reader, err := ringbuf.NewReader(tracer.objs.Events)
	if err != nil {
		_ = kprobe.Close()
		_ = tracer.objs.Close()
		return nil, fmt.Errorf("opening ring buffer: %w", err)
	}
	tracer.reader = reader

	return tracer, nil
}

// Read blocks until the next OOM kill is reported, returning a copy of the
// sample. It returns ErrClosed once the tracer is closed.
func (t *Tracer) Read() ([]byte, error) {
	record, err := t.reader.Read()
	if err != nil {
		return nil, err
	}

	// RawSample points into the ring buffer and is reused by the next Read.
	sample := make([]byte, len(record.RawSample))
	copy(sample, record.RawSample)

	return sample, nil
}

// Close detaches the probe and releases the ring buffer. Safe to call once; a
// blocked Read is unblocked with ErrClosed.
func (t *Tracer) Close() error {
	var errs []error
	if t.reader != nil {
		errs = append(errs, t.reader.Close())
	}
	if t.kprobe != nil {
		errs = append(errs, t.kprobe.Close())
	}
	errs = append(errs, t.objs.Close())

	return errors.Join(errs...)
}

// verifierDetail expands a rejection by the kernel verifier into its full log.
//
// Without this the error is a bare "permission denied", which says nothing
// about which instruction the verifier objected to. The log is the only
// actionable thing a bug report can contain.
func verifierDetail(err error) error {
	var verr *ebpf.VerifierError
	if errors.As(err, &verr) {
		// %+v is the only verb that prints the whole log. VerifierError.Error,
		// which is what %w and %v use, deliberately summarises it, and the
		// summary drops the very instruction the verifier rejected.
		//nolint:errorlint // Wrapping here would discard the log this function exists to surface.
		return fmt.Errorf("%+v", verr)
	}
	return err
}
