# Toolchain for regenerating the eBPF objects in internal/detector/bpf.
#
# It exists because Apple's clang has no BPF backend ("No available targets are
# compatible with triple bpf"), so codegen cannot run natively on a Mac. Pinning
# it in an image also means CI and a developer laptop produce byte-identical
# objects, which is the only way `git diff` after `just bpf-generate` is a
# meaningful check.
#
# Nothing at runtime needs this image. The generated .o and .go files are
# committed, so `go build` works with nothing installed but Go.
FROM golang:1.26-bookworm

# clang/llvm compile the BPF C. libbpf-dev supplies bpf_helpers.h and
# bpf_core_read.h. No bpftool: the probe declares its own kernel types instead
# of dumping a vmlinux.h, so codegen needs no kernel BTF and stays reproducible.
RUN apt-get update \
    && apt-get install --no-install-recommends -y \
        clang \
        llvm \
        libbpf-dev \
    && rm -rf /var/lib/apt/lists/*

# linux/types.h includes <asm/types.h>, which Debian keeps under a multiarch
# directory that clang does not search. Linking it to the canonical location
# keeps the compiler flags identical whether codegen runs on an arm64 laptop or
# an amd64 CI runner. The header itself is just fixed-width typedefs, so both
# architectures resolve to the same declarations.
RUN set -eux; \
    multiarch="$(ls -d /usr/include/*-linux-gnu | head -n 1)"; \
    ln -sfn "${multiarch}/asm" /usr/include/asm; \
    test -r /usr/include/asm/types.h

# Pinned so the generated Go changes only when this file does.
RUN go install github.com/cilium/ebpf/cmd/bpf2go@v0.20.0

WORKDIR /src
