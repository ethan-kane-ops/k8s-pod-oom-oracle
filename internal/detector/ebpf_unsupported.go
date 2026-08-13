//go:build !linux || (!amd64 && !arm64)

package detector

import "fmt"

// NewEBPF reports that this build has no kernel probe.
//
// The build constraint is the exact complement of ebpf_supported.go, so every
// platform gets one of the two and the daemon compiles everywhere. Developing
// on a Mac is the point: only the detector is unavailable, not the tool.
func NewEBPF(EBPFOptions) (Detector, error) {
	return nil, fmt.Errorf("%w: no kernel probe is compiled for this platform", ErrEBPFUnsupported)
}
