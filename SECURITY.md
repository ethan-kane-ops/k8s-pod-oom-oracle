# Security Policy

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security-related reports.

Report vulnerabilities privately via the GitHub Security Advisory page for this
repository:

https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/security/advisories/new

When submitting a report, please include:

- **Affected component**: the eBPF probe, the cgroup or procfs readers, the
  correlation layer, the HTTP API, or the deployment manifests.
- **Description**: what the vulnerability is and its potential impact.
- **Reproduction**: step-by-step instructions, including kernel version, cgroup
  version, and the manifests or flags used.
- **Mitigation**: any temporary workarounds you have identified.

### Response targets

| Stage | Target |
|---|---|
| Acknowledge receipt | 48 hours |
| Triage and confirm severity | 7 days |
| Fix released for critical severity | 14 days |

Reporters will be credited in the release notes for the fix unless they request
anonymity.

## What this daemon can do on a node

This is not a sidecar. It is a privileged node agent, and its threat model
should be read before deploying it. What it needs, and why, is documented in the
README; what matters here is what those privileges mean if it is compromised.

| What `deploy/20-daemonset.yaml` asks for | Grants an attacker |
|---|---|
| `privileged: true` with `runAsUser: 0` | Root on the node, for practical purposes |
| `hostPID: true` | Every process on the node, through `/proc` |
| `/sys/fs/cgroup` and `/proc` host mounts | Memory accounting and command lines for every container |
| `pods: get, list, watch` cluster-wide | Pod metadata for the entire cluster |

The first row is the one to weigh. `privileged` is stronger than this daemon
conceptually needs, and the manifest says why next to the field: the image runs
as `nonroot`, and a non-zero UID starts with an empty effective capability set
regardless of its bounding set, so `bpf()` returns `EPERM` and the daemon
silently degrades to polling. Narrowing this to `CAP_BPF` and `CAP_PERFMON` on a
kernel that supports them is on the [roadmap](./ROADMAP.md); it is not a
supported configuration today.

Three deliberate limits narrow that surface:

- **The probe is read-only.** It attaches a kprobe to `oom_kill_process` and
  copies fields out of kernel structures. It writes nothing to the kernel, makes
  no scheduling or kill decisions, and cannot alter the outcome of an OOM event.
- **The host filesystem mounts are read-only**, including `/sys/fs/cgroup`.
- **RBAC is one rule.** The node name arrives through the downward API rather
  than a `nodes` read, specifically so the ClusterRole stays a single line. See
  `deploy/10-rbac.yaml`, which says the same thing next to the grant itself.

The API server binds to a container port with no authentication. It serves
process names, command lines and memory figures for workloads on its node, which
is enough to fingerprint what a cluster runs. Do not expose it beyond the node
without putting authentication in front of it, and prefer scraping it through
the API server proxy or a NetworkPolicy-restricted path.

## Supported Versions

This project has not yet cut a release. Until it does, only `main` is supported,
and security fixes land there.

Once releases begin, the latest minor line will receive security updates and
this table will be filled in.

## Signed Releases

Not yet. Release signing (cosign keyless, bound to this repository's release
workflow) is planned alongside the first tagged release. This section will
document the exact `cosign verify` command when the signing pipeline lands.

Until then, build from source. `go build` needs only Go: the eBPF objects are
committed, so no toolchain beyond the Go compiler is involved in producing the
binary you run.
