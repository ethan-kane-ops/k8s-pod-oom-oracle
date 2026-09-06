# Kubernetes Pod OOM Oracle 🔮

[![CI](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/actions/workflows/ci.yml/badge.svg)](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-mkdocs--material-0a9edc.svg)](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/oom-oracle)](https://artifacthub.io/packages/search?repo=oom-oracle)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ethan-kane-ops/k8s-pod-oom-oracle/badge)](https://scorecard.dev/viewer/?uri=github.com/ethan-kane-ops/k8s-pod-oom-oracle)
[![eBPF Powered](https://img.shields.io/badge/eBPF-Kernel--level-red.svg)](https://ebpf.io)

A node agent that explains Kubernetes OOM kills at the level the control plane
cannot: which process died, how much memory it held at the moment the kernel
chose it, what the memory curve looked like on the way there, and what else was
in the container when it happened.

**📖 [Full documentation](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/)**

![oom-oracle explaining an OOM kill and browsing a node's kills live](docs/assets/demo.gif)

---

## The problem

Kubernetes tells you `OOMKilled` and exit code 137, and then stops. It cannot
tell you which of the five processes in that container took the memory, what it
held at the moment the kernel picked it, or whether usage climbed for an hour or
spiked in a second. On a pool of workers that all exec the same binary, "which
one" is the entire question, and the answer is on the node, in a kernel ring
buffer you probably cannot reach.

OOM Oracle attaches a kprobe to the kernel's `oom_kill_process`, samples cgroup
memory continuously so a trajectory already exists when the kill lands, and
resolves the cgroup path back to a namespace, pod and container name.

```
POD: payment-api-6d5f78 (namespace: default)
CONTAINER: web-server (image: payment:v1.2.0)
QOS: Burstable

DIAGNOSIS: OOMKilled (2026-08-13 08:15:22 UTC)
  Limit:        512.0MiB
  Peak usage:   512.0MiB
  Kill count:   1
  Detected by:  ebpf
  Growth rate:  1.0MiB/s (fit R²=0.97 over 1m0s)

MEMORY TRAJECTORY (last 1m0s):
  08:14:22:  412.0MiB / 512.0MiB  [██████████████░░░░]  80%
  08:14:37:  460.0MiB / 512.0MiB  [████████████████░░]  90%  (stall 12%)
  08:14:52:  498.0MiB / 512.0MiB  [█████████████████░]  97%  (stall 24%)
  08:15:07:  507.0MiB / 512.0MiB  [█████████████████░]  99%  (stall 36%)
  08:15:22:  512.0MiB / 512.0MiB  [██████████████████] 100%  (stall 48%)

VICTIM PROCESS:
  PID:             28145 (in container: 17)
  Command:         node ./dist/garbage-collector.js
  Exit code:       137 (OOM)
  Memory at death: 114.0MiB
  Confidence:      traced in the kernel at the moment of the kill.

PROCESSES IN CONTAINER AFTER THE KILL:
  memory.oom.group=0: the kernel killed only the process it
  selected, so these were still running after it died.
  1. node ./dist/server.js (PID 28102) - 390.0MiB
  2. node ./dist/worker.js (PID 28160) - 8.0MiB
```

---

## Quickstart

### With Helm

```bash
helm install oom-oracle oci://ghcr.io/ethan-kane-ops/charts/oom-oracle \
  --namespace oom-oracle --create-namespace
kubectl label namespace oom-oracle pod-security.kubernetes.io/enforce=privileged
```

The chart is on [Artifact Hub](https://artifacthub.io/packages/helm/oom-oracle/oom-oracle).
It and the image it installs are signed with cosign in keyless mode; `SECURITY.md`
has the verification commands.

The `privileged` Pod Security label is not optional. `hostPID`, `hostPath`
volumes and non-default capabilities are each outside `baseline`, so without it
admission rejects the pods and the DaemonSet reports no event explaining why.

### On kind, end to end

```bash
just e2e-deploy   # create the cluster, build the image, load it, roll out the DaemonSet
```

### Without Helm

`deploy/` is the same DaemonSet the chart renders, against
`ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:latest`. Pin a tag or a digest before
using it on anything you care about: `latest` moves on every release, and this
is a privileged node agent.

```bash
kubectl apply -f deploy/
kubectl -n oom-oracle rollout status daemonset/oom-oracle
```

To run your own build instead, push it somewhere the nodes can reach and point
the DaemonSet at it:

```bash
docker build -t <your-registry>/oom-oracle:dev .
docker push <your-registry>/oom-oracle:dev
kubectl -n oom-oracle set image daemonset/oom-oracle oom-oracle=<your-registry>/oom-oracle:dev
```

### Read the reports

```bash
kubectl -n oom-oracle port-forward pod/<agent-pod> 9090:9090
oom-oracle watch                        # live dashboard
oom-oracle inspect                      # every recorded kill, newest first
oom-oracle inspect payment-api-6d5f78   # one pod
oom-oracle inspect -n default -o json   # filtered, machine-readable
```

[Getting Started](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/getting-started/)
covers this in full, including how to trigger a kill with the sample workloads
in [`examples/workloads/`](examples/workloads/).

---

## Documentation

| | |
|---|---|
| [Getting Started](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/getting-started/) | Install, deploy, trigger a kill, read the report |
| [Dashboard](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/dashboard/) | The live terminal view, and its keys |
| [Detectors](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/detectors/) | eBPF versus polling, and why one is a fallback |
| [Correlation](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/correlation/) | Cgroup path to pod name, the informer, and RBAC |
| [Configuration](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/configuration/) | Every flag on every command |
| [Deployment](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/deployment/) | DaemonSet, privileges, Pod Security, resource sizing |
| [API Reference](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/reference/api/) | Routes and the full report schema |
| [Troubleshooting](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/troubleshooting/) | When the probe will not attach, and what the counters mean |
| [Security](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/security/) | Threat model, the capabilities it asks for, what the probe reads |
| [Development](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/development/) | Build, eBPF regeneration, the e2e suite |
| [Releasing](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/releasing/) | What a tag publishes, and how to cut one |

---

## Project status

Released and installable. `v0.1.1` publishes signed CLI archives, a multi-arch
image on `ghcr.io`, and a Helm chart listed on Artifact Hub.

Pre-1.0, so the HTTP API and report JSON can still change shape. Breaking
changes are written by hand in [CHANGELOG.md](./CHANGELOG.md) with the migration
each one needs, because a commit subject cannot tell you which JSON field to
rename.

Working today: both detectors, cgroup v1 and v2 sampling, pod-name correlation,
the HTTP API, the text and JSON renderers, the terminal dashboard, and an e2e
suite that runs on kind in CI.

[ROADMAP.md](./ROADMAP.md) has the full picture, including the known limitations
worth reading before you deploy this on anything you care about.

---

## Contributing

Issues and pull requests are welcome.

| | |
|---|---|
| [CONTRIBUTING.md](./CONTRIBUTING.md) | Development setup, task runner, and what a PR should contain |
| [ROADMAP.md](./ROADMAP.md) | Direction, and the limitations this tool currently has |
| [SUPPORT.md](./SUPPORT.md) | Where to ask, and what to include so it can be answered once |
| [SECURITY.md](./SECURITY.md) | Private disclosure, and what this daemon's privileges mean on a node |
| [CODE_OF_CONDUCT.md](./CODE_OF_CONDUCT.md) | Contributor Covenant 2.1 |

---

## License

Distributed under the Apache License 2.0. See `LICENSE` for details.

`internal/detector/bpf/oomtracer.bpf.c` is the one exception: it is GPL-2.0, as
marked by its SPDX header. It calls GPL-only BPF helpers, and the kernel refuses
to load a program that declares any other licence. It is compiled to a BPF
object and loaded into the kernel, not linked into the Go binary.
