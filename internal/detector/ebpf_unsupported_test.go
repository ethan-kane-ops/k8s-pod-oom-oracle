//go:build !linux || (!amd64 && !arm64)

package detector

import (
	"errors"
	"testing"
)

func TestNewEBPFUnsupported(t *testing.T) {
	t.Parallel()

	// The daemon distinguishes "this host cannot trace" from "tracing broke" by
	// matching on ErrEBPFUnsupported, and falls back to polling only for the
	// former. A bare error here would turn a Mac build into a hard failure
	// instead of a poller.
	got, err := NewEBPF(EBPFOptions{CgroupRoot: "/sys/fs/cgroup"})
	if err == nil {
		t.Fatal("NewEBPF() succeeded on a platform with no compiled probe")
	}
	if !errors.Is(err, ErrEBPFUnsupported) {
		t.Errorf("NewEBPF() error = %v, want it to wrap ErrEBPFUnsupported", err)
	}
	if got != nil {
		t.Errorf("NewEBPF() returned a detector alongside an error: %v", got)
	}
}
