# Development

## Prerequisites

* Go 1.26.5 (pinned in `.mise.toml`)
* [mise](https://mise.jdx.dev/) for runtimes, [just](https://just.systems/) for tasks
* Docker, for the e2e suite and for regenerating the eBPF objects
* A Linux kernel ≥ 5.8 with BTF, to run the eBPF detector. macOS builds and
  tests everything except the probe itself

## Get started

```bash
mise install
just build      # → bin/oom-oracle
just check      # tidy + lint (both target platforms) + race tests
```

`just` with no arguments lists every target.

## The eBPF objects are committed

The compiled objects in `internal/detector/bpf` are checked in, so a plain
`go build` needs only Go. After editing the BPF C, regenerate them:

```bash
just bpf-generate  # compiles in a pinned container
just bpf-verify    # fails if the committed objects are stale (also runs in CI)
```

This runs in Docker because Apple's clang has no BPF backend. Developing on a
Mac otherwise works normally: the detector is unavailable there, the rest of the
tool is not.

!!! warning "Commit the regenerated objects"
    CI's `bpf-verify` job fails if the committed objects do not match the C
    source. Regenerating without committing produces a red build that names the
    objects rather than the edit.

## Linting targets two platforms

```bash
just lint
```

Four passes: `go vet` and `golangci-lint` against both `GOOS=linux` and
`GOOS=darwin`.

The eBPF detector and its non-Linux fallback sit behind mutually exclusive build
tags, so a single-platform run always leaves one of them uncompiled and
therefore unchecked. A lint run that only covers one `GOOS` will pass over real
errors in the other file.

## Tests

```bash
just test        # go test ./...
just test-race   # with the race detector
just test-cover  # with a per-package coverage summary
just fuzz 60s    # every fuzz target, 60 seconds each
```

Tests are table-driven and use `t.TempDir()` for filesystem fixtures. The cgroup
and procfs readers are exercised against `fstest.MapFS` trees, so the parsers run
on any platform including machines with no cgroup support.

## End-to-end tests

The unit suite proves the parsers. The e2e suite proves the product: it deploys
the daemon to a kind cluster, makes real pods exceed real limits, and asserts on
the post-mortems the deployed daemon produced.

```bash
just e2e       # create the cluster, deploy, run the suite
just e2e-logs  # the deployed daemon's logs, when a run fails
just e2e-down  # tear the cluster down
```

The suite is behind a build tag, so `go test ./...` stays a unit run.

!!! tip "This is where the real bugs are found"
    Every bug that has mattered in this project was found this way rather than
    by a unit test, including two in the suite's first run: the agent silently
    losing all capabilities because the distroless base runs as non-root, and
    every pod on a kubelet with its own cgroup root being classified as
    `Unknown` QoS.

    A fixture cannot reproduce either.

## The docs site

```bash
just docs-serve   # live reload on http://127.0.0.1:8000
just docs-build   # build into ./site, --strict
```

`--strict` turns a broken internal link into a build failure. The nav is the only
index of these pages, so a silently dead link is a page nobody reaches.

Dependencies are hash-pinned in `docs/requirements.txt`. To change them, edit
`docs/requirements.in` and recompile:

```bash
uv pip compile docs/requirements.in --generate-hashes --universal -o docs/requirements.txt
```

## Conventions

* Error strings: lowercase, no trailing punctuation, wrapped with `%w`
* New subcommands go in `internal/cmd/`, registered in `root.go`'s `init()`
* `just check` must pass before every commit
* Conventional commits: `type(scope): message`

## Before opening a PR

```bash
just check
```

[CONTRIBUTING.md](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/CONTRIBUTING.md)
has the full expectations for what a pull request should contain.
