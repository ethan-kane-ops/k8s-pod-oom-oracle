# Support

Thanks for using oom-oracle. Here is where to go depending on what you need.

## Documentation

- [README.md](./README.md): install, flags, API routes, and what the daemon
  needs on a node.
- [CONTRIBUTING.md](./CONTRIBUTING.md): development setup and the task runner.

## Questions and usage help

Open a [GitHub issue](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/issues/new/choose).
Please search existing issues first.

## Bugs

File a bug with the bug-report template at
https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/issues/new/choose.

For anything involving a missed, wrong, or unresolved OOM report, **the
environment is the bug**. The template asks for kernel version, cgroup version,
BTF availability, container runtime and the daemon's own `/v1/status` output,
because a report that omits them cannot be triaged without a round trip. The
single most useful thing you can attach is the daemon's startup log, which
states which detector attached and why.

## Security vulnerabilities

Do **not** open a public issue for a security problem. Report it privately via a
[GitHub Security Advisory](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/security/advisories/new).
See [SECURITY.md](./SECURITY.md) for the disclosure policy, and for what this
daemon's privileges mean on a node.
