# oom-oracle

Node agent that explains Kubernetes OOM kills at the process level, using eBPF
and cgroups.

Kubernetes reports `OOMKilled` and exit code 137, and stops. This says which
process in the container died, what it held when the kernel chose it, and what
the memory curve looked like on the way there.

## Install

```bash
helm install oom-oracle oci://ghcr.io/ethan-kane-ops/charts/oom-oracle \
  --namespace oom-oracle --create-namespace
```

The namespace must carry the `privileged` Pod Security level, or admission
rejects the pods and the DaemonSet reports no event explaining why:

```bash
kubectl label namespace oom-oracle \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged
```

This is required even though the agent is not `privileged: true`. `hostPID`,
`hostPath` volumes and non-default capabilities are each outside `baseline` on
their own.

## What it asks for, and why

| Grant | Why |
|---|---|
| `CAP_BPF` | Creating the maps and loading the verified program |
| `CAP_PERFMON` | Attaching the kprobe. Loading succeeds without it; attaching is what fails |
| `runAsUser: 0` | A non-zero UID starts with an empty *effective* capability set however large its bounding set is, and a pod spec cannot request the ambient capabilities that would populate it |
| `hostPID` | `/proc` must show the node's processes, not the pod's |
| `/sys/fs/cgroup`, `/proc` | Read-only host mounts. The daemon never writes to the node |
| `pods: get,list,watch` | The whole of its cluster access, and what turns a pod UID into a name |

None of these is a value. A chart that let you turn off `hostPID` would produce
a daemon that installs, runs, reports nothing, and explains nothing.

The agent is not `privileged: true`. It runs with `drop: [ALL]`,
`allowPrivilegeEscalation: false`, a read-only root filesystem and the
`RuntimeDefault` seccomp profile.

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle` | Image repository |
| `image.tag` | `""` | Defaults to `.Chart.AppVersion` |
| `image.digest` | `""` | Pin by digest instead of tag |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `nameOverride` / `fullnameOverride` | `""` | |
| `daemon.detector` | `auto` | `auto`, `poller` or `ebpf`. `auto` falls back and logs why; `ebpf` refuses to start rather than infer |
| `daemon.pollInterval` | `1s` | Poller only; the eBPF detector is event-driven |
| `daemon.sampleInterval` | `1s` | How often memory is sampled for the trajectory |
| `daemon.history` | `60` | Samples retained per container |
| `daemon.retain` | `256` | Reports held in memory. There is no persistence |
| `daemon.cgroupPrefix` | `/` | Limit watching to a subtree |
| `daemon.includeNonKubernetes` | `false` | Also report kills outside the kubepods tree |
| `daemon.kubernetes` | `auto` | `auto`, `on` or `off`. `off` yields UID-only reports |
| `daemon.logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `daemon.logFormat` | `json` | `text` or `json` |
| `daemon.listen` | `:9090` | Bound inside the pod, not on the node |
| `daemon.extraArgs` | `[]` | Appended verbatim |
| `service.enabled` | `true` | |
| `service.port` | `9090` | |
| `serviceAccount.create` | `true` | |
| `rbac.create` | `true` | Disabling this yields UID-only reports |
| `resources` | 20m CPU / 64Mi, 128Mi limit | |
| `tolerations` | `[{operator: Exists}]` | An untolerated taint is a node with no OOM reporting and nothing saying so |
| `nodeSelector`, `affinity` | `{}` | |
| `podAnnotations`, `podLabels` | `{}` | Added to the pod template |
| `livenessProbe`, `readinessProbe` | 5s/20s, 2s/5s | `/readyz` means the probe is attached and history is being kept, not merely that the process started |
| `priorityClassName` | `system-node-critical` | Keeps the agent from being evicted at the moment it exists for |
| `updateStrategy` | RollingUpdate, maxUnavailable 1 | |
| `hostPaths.cgroup` | `/sys/fs/cgroup` | |
| `hostPaths.proc` | `/proc` | |

## The Service is headless

Each agent holds only its own node's reports. A load-balanced VIP would answer a
query about one node's pod from a different node's daemon and report nothing
found, so callers resolve individual pods instead.

## Read the reports

```bash
kubectl -n oom-oracle port-forward daemonset/oom-oracle 9090:9090
oom-oracle watch      # live dashboard
oom-oracle inspect    # every recorded kill, newest first
```

Check `detector` in `/v1/status` before trusting a victim. `ebpf` means it was
traced in the kernel as it was killed; `poller` means it was inferred from which
process vanished between samples.

## Links

- [Documentation](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/)
- [Source](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle)
- [Security](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/SECURITY.md)
