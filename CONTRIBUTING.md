# Contributing to k8s-pod-oom-oracle

Thanks for taking the time. This document covers the development workflow,
coding standards, and what a pull request is expected to contain.

---

## Development Setup

The project uses [mise](https://mise.jdx.dev/) for language runtimes and
[just](https://just.systems/) as the task runner.

### Prerequisites

1. Install **mise**: see https://mise.jdx.dev/installing-mise.html
2. Activate it in your shell: see https://mise.jdx.dev/getting-started.html#activate-mise
3. Provision the pinned tools (Go, golangci-lint, just):
   ```bash
   mise install
   ```
4. Install **pre-commit**: see https://pre-commit.com/#install
5. Enable the hooks:
   ```bash
   pre-commit install
   ```

**Docker** is needed for two things only: the end-to-end suite, and
regenerating the eBPF objects. Neither is required for a normal build.

### Platform notes

macOS builds, lints and tests everything except the eBPF probe itself, which
needs a Linux kernel ≥ 5.8 with BTF. The compiled BPF objects are committed, so
a plain `go build` never needs clang on any platform.

---

## Common Targets

`just` with no arguments lists them all. The ones you will use:

| Target | Purpose |
|---|---|
| `just build` | Compile to `bin/oom-oracle` |
| `just test` | Unit tests |
| `just test-race` | Unit tests with the race detector |
| `just test-cover` | Tests plus a per-package coverage summary |
| `just lint` | `go vet` and `golangci-lint`, both target platforms |
| `just check` | tidy + lint + race tests. **Required before every commit** |
| `just fuzz [time]` | Fuzz every parser target for the given budget each, default 60s |
| `just e2e` | Create a kind cluster, deploy, run the end-to-end suite |
| `just e2e-logs` | The deployed daemon's logs, for when an e2e run fails |
| `just e2e-down` | Tear the kind cluster down |
| `just bpf-generate` | Regenerate the eBPF objects in a pinned container |
| `just bpf-verify` | Fail if the committed objects are stale. Also runs in CI |

### Two things that catch people out

**`just lint` runs four passes, and all four matter.** The eBPF detector and its
non-Linux fallback sit behind mutually exclusive build tags. A run on one `GOOS`
never compiles the other file, so a single pass always leaves half the package
unchecked. This is not hypothetical: it is how an `errorlint` failure reached
`main`. The Linux pass also carries `--build-tags e2e`, because without it
`test/e2e` is invisible to every linter in the repo.

**The eBPF objects are committed.** After editing
`internal/detector/bpf/oomtracer.bpf.c` you must run `just bpf-generate` and
commit the regenerated `.o` and `.go` files, or CI's `bpf-verify` job fails.
Generation runs in a container because Apple's clang has no BPF backend.

---

## Coding Guidelines

### Errors

- Lowercase, no trailing punctuation.
- Wrap with `%w` to preserve the cause: `fmt.Errorf("reading cgroup stats: %w", err)`.

### Tests

- Table-driven for anything with more than two logical branches.
- `t.TempDir()` for filesystem fixtures.
- No `time.Sleep` waits. Use channels, injected clocks, or a polling helper with
  a deadline.
- No network dependencies. Use `httptest.Server` or a fake.
- Kubernetes tests use `fake.NewClientset`. Note that the fake **does not
  enforce field or label selectors on List**, so a test that seeds pods on two
  nodes and expects the informer to see one will pass no matter what the code
  does. Assert on the recorded action's `GetListRestrictions()` instead.

### Commands

New subcommands go in `internal/cmd/` and are registered in `root.go`'s
`NewRootCmd`.

---

## Commit Messages

[Conventional Commits](https://www.conventionalcommits.org/), which drive
changelog generation via [git-cliff](https://git-cliff.org/).

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

**Types**: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `chore`,
`ci`, `revert`.

**Examples**:

```
feat(detector): attach a kprobe to oom_kill_process
fix(correlate): normalise cgroup paths read from a private namespace
test(k8s): assert the informer's field selector on recorded actions
```

Summary in imperative mood, under 72 characters.

---

## Pull Requests

1. Fork and branch from `main`:
   ```bash
   git checkout -b feat/short-description
   ```
2. Make focused commits, one logical change each.
3. Run `just check`. If you touched anything the daemon does on a node, also run
   `just e2e`.
4. Push and open a pull request against `main`.
5. CI must pass and a maintainer must approve before merge.

### Writing the description

Assume the reader is a stranger reviewing a public repository who was not part
of any conversation that produced the change. State what changed and why it
changed. Leave out the journey: failed attempts, debugging steps and
intermediate CI failures belong nowhere in the final description.

---

## Reporting Bugs

For anything involving a missed or wrong OOM report, the environment is the bug,
so please include:

- Kernel version (`uname -r`)
- cgroup version (`stat -fc %T /sys/fs/cgroup`)
- Whether BTF is present (`ls /sys/kernel/btf/vmlinux`)
- Container runtime and Kubernetes version
- The output of `curl localhost:9090/v1/status` from the affected agent
- The daemon's startup logs, which state which detector attached and why

For security vulnerabilities, do not open a public issue. Report them privately
through this repository's GitHub Security Advisories page.
