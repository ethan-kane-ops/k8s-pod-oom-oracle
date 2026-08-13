// Package bpf holds the compiled OOM tracer and the loader bindings bpf2go
// generates for it.
//
// The .o and _bpf*.go files in this directory are generated but committed, so
// building this project needs nothing but a Go toolchain. Regenerating them
// needs clang with a BPF backend, which macOS does not ship: run
// `just bpf-generate`, which does it inside the pinned image in build/bpf.
//
// Everything here is constrained to Linux. On other platforms the package is
// empty apart from this file, and internal/detector falls back to a stub that
// reports eBPF as unsupported.
package bpf

//go:generate bpf2go -go-package bpf -target amd64,arm64 -tags linux -type oom_event oomtracer oomtracer.bpf.c -- -O2 -g -Wall -Werror
