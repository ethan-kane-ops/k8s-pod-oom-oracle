# Security

!!! info "Reporting a vulnerability"
    Do not open a public issue. Use the
    [GitHub Security Advisory page](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/security/advisories/new).
    Full policy and response targets are in
    [SECURITY.md](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/SECURITY.md).

## What this daemon can do on a node

It runs `privileged: true` as UID 0, with `hostPID` and the host's
`/sys/fs/cgroup` and `/proc` mounted. That is a lot of authority, and it is worth
being precise about what it is used for.

| Capability | Used for | Not used for |
|---|---|---|
| Loading a BPF program | One kprobe on `oom_kill_process` | Nothing else. No network hooks, no LSM, no tracepoints on syscalls |
| `hostPID` | Reading `/proc/<pid>` for a victim's command line and the container's process list | Signalling, killing, or entering any namespace |
| Host mounts | Reading cgroup memory files | Both are mounted read-only |
| `pods: get,list,watch` | Turning a pod UID into a name | Nothing is written to the API server |

The daemon never writes to the node or to the API server.

## Why privileged, and what would replace it

`privileged: true` is stronger than the work needs. The job wants `CAP_BPF` and
`CAP_PERFMON`, not everything.

Narrowing it is open work. The obstacle is that a non-root UID starts with an
empty effective capability set no matter how permissive its bounding set is, so
the failure mode is a silent fallback to polling rather than an error. Shipping
a narrower profile that quietly degrades on some kernels would be worse than the
honest broad one.

Until that lands, the mitigation is scope: the DaemonSet is one namespace, one
ServiceAccount, and one ClusterRole containing a single read-only rule.

## What the probe reads

The kprobe fires on entry to `oom_kill_process` and reads, from the victim the
kernel has already selected:

- its PID and PID namespace
- its `comm`, the kernel's 15-character executable name
- its resident set size, out of its own `mm`
- its cgroup path

It then reads `/proc/<pid>` from userspace to recover the full command line
before `SIGKILL` lands.

**Command lines can contain secrets.** A process started with a credential in
`argv` will have that credential in `victim.cmdline`, and therefore in the
report, the API response, and the rendered post-mortem. This is not specific to
this tool, since `/proc` already exposes it to anything on the node, but it is
worth knowing before pointing a log shipper at the API.

## The HTTP API is unauthenticated

There is no authentication, no TLS, and no authorization.

This is deliberate for a node-local agent, and unacceptable the moment it is
exposed further. The Service is headless, so nothing load-balances it by default,
and the DaemonSet does not use `hostNetwork` or a `hostPort`.

If you need it reachable beyond the node, put an authenticating proxy in front
and do not publish the port.

## Output handling

Reports carry values read from the node's filesystem: cgroup paths and process
command lines, neither of which the daemon controls. JSON responses are served
with an explicit `Content-Type: application/json` and
`X-Content-Type-Options: nosniff` so a browser can never interpret that content
as markup.

The text renderer strips control characters, so a hostile or merely messy command
line cannot corrupt a terminal.

## Supply chain

No release has been tagged and no artifacts are published, so there is nothing to
verify yet. Signed multi-arch images with an SBOM are on the roadmap, and
`SECURITY.md` does not promise verifiable artifacts until that lands.

The eBPF objects in `internal/detector/bpf` are committed to the repository and
built in a pinned container. CI's `bpf-verify` job fails if the committed objects
do not match the C source, so a tampered object cannot ride in unnoticed.

## Licence note

`internal/detector/bpf/oomtracer.bpf.c` is GPL-2.0, unlike the Apache-2.0 rest of
the project. It calls GPL-only BPF helpers, and the kernel refuses to load a
program declaring any other licence. It is compiled to a BPF object and loaded
into the kernel, not linked into the Go binary.
