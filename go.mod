module github.com/ethan-kane-ops/k8s-pod-oom-oracle

// The patch version is pinned deliberately: CI installs from this file, so it
// is what keeps govulncheck clear of stdlib advisories. It must never exceed
// the Go shipped in golang:1.26, because the official images set
// GOTOOLCHAIN=local and refuse to download a newer toolchain.
go 1.26.5

require (
	github.com/cilium/ebpf v0.22.0
	github.com/spf13/cobra v1.10.2
	golang.org/x/sync v0.22.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)
