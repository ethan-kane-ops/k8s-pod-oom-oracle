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
| `runAsUser: 0` with `CAP_BPF` and `CAP_PERFMON` | Loading BPF programs and attaching probes, as root inside the container |
| `hostPID: true` | Every process on the node, through `/proc` |
| `/sys/fs/cgroup` and `/proc` host mounts | Memory accounting and command lines for every container |
| `pods: get, list, watch` cluster-wide | Pod metadata for the entire cluster |

The first row is the one to weigh. It is narrower than it was: the manifest
shipped `privileged: true` until the minimum was measured on a real cluster, and
now asks for two capabilities with `drop: [ALL]`, `allowPrivilegeEscalation:
false`, a read-only root filesystem and the `RuntimeDefault` seccomp profile.

It is still UID 0. A non-zero UID starts with an empty effective capability set
regardless of its bounding set, and populating it needs ambient capabilities that
a pod spec cannot request, so `bpf()` would return `EPERM` and the daemon would
silently degrade to polling. Running as `nonroot` is therefore not available;
running unprivileged is, and it does.

The namespace still needs the `privileged` Pod Security level, because `hostPID`,
`hostPath` volumes and non-default capabilities are each outside `baseline`.

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

Until the first tag is cut, only `main` is supported and security fixes land
there. From the first release, the latest minor line receives security updates.

## Signed Releases

Everything a tag publishes is signed with [cosign](https://docs.sigstore.dev) in
keyless mode. There is no public key to fetch and no private key held anywhere:
the signing certificate is issued to the release workflow's OIDC identity and
expires in minutes, so verification asks who signed it rather than which key.

Substitute the tag you downloaded for `v0.1.0` throughout.

### The container image

```bash
cosign verify \
  --certificate-identity-regexp '^https://github\.com/ethan-kane-ops/k8s-pod-oom-oracle/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:v0.1.0
```

Both flags are required. Without them cosign accepts a signature from any
identity Sigstore will issue a certificate to, which is anyone at all.

Build provenance is attested through GitHub and can be checked with the `gh`
CLI, which names the workflow and commit the image was built from:

```bash
gh attestation verify oci://ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:v0.1.0 \
  --repo ethan-kane-ops/k8s-pod-oom-oracle
```

### The CLI archives

The checksum file is signed, and the archives are covered by it. Download
`checksums.txt` and `checksums.txt.bundle` from the release:

```bash
cosign verify-blob \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github\.com/ethan-kane-ops/k8s-pod-oom-oracle/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

sha256sum --check --ignore-missing checksums.txt
```

The bundle holds the signature, the signing certificate and the transparency
log entry in one file. Releases before v0.1.1 are not signed at all: v0.1.0's
signing step failed, so that tag published an image and a chart but no archives.

Verify the checksum file first. Checking an archive against an unverified
checksum file proves only that the two came from the same place.

### SBOMs

Every archive ships an SPDX document beside it, and the image carries one in its
manifest, readable with `cosign download sbom` or `syft`.

### Or build from source

`go build` needs only Go. The eBPF objects are committed, so no toolchain beyond
the Go compiler is involved in producing the binary you run.
