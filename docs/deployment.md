# Deployment

The `deploy/` directory contains four manifests, applied in order by
`kubectl apply -f deploy/`.

| File | Contents |
|---|---|
| `00-namespace.yaml` | The `oom-oracle` namespace, labelled for Pod Security |
| `10-rbac.yaml` | ServiceAccount, ClusterRole, ClusterRoleBinding |
| `20-daemonset.yaml` | The agent itself |
| `30-service.yaml` | A headless Service |

## What it needs, and why

| Grant | Why |
|---|---|
| `CAP_BPF` | Creating the maps and loading the verified program |
| `CAP_PERFMON` | Attaching the kprobe through `perf_event_open`. Loading succeeds without it; attaching is what fails |
| `runAsUser: 0` | A non-root process starts with an empty effective capability set regardless of its bounding set, so `bpf()` returns `EPERM` and the daemon silently degrades |
| `hostPID: true` | `/proc` must show the node's processes, or there is no victim to identify and no process list to build |
| `/sys/fs/cgroup`, `/proc` | Read-only host mounts. The daemon never writes to the node |
| `pods: get,list,watch` | The only cluster access it has. Read-only, and only to turn a pod UID into a name |

The daemon never writes to the node or to the API server.

!!! warning "runAsUser: 0 is not redundant"
    Capabilities alone are not enough. The distroless base image runs as a
    non-root user, and a non-root process starts with an empty *effective*
    capability set no matter how permissive its bounding set is. Populating it
    for a non-root UID needs ambient capabilities, which a pod spec cannot
    request. The result is not an error: `bpf()` returns `EPERM` and the daemon
    quietly falls back to polling. This was found by the e2e suite, not by a
    unit test.

    The container therefore runs as UID 0 with `drop: [ALL]` and exactly two
    capabilities added, rather than as a non-root user holding two capabilities
    it could never use.

!!! note "`privileged: true` is no longer requested"
    It was, until the narrower set was measured on kind. Dropping it did break
    the process listing at first, and the cause was not a capability: containerd
    puts a privileged container in the host cgroup namespace and everything else
    in a private one, and `/proc/<pid>/cgroup` is written relative to the
    reader's. The daemon now reads cgroup membership from the kernel's
    `cgroup.procs`, which reads the same from any namespace.

## Pod Security Admission

The namespace carries the `privileged` PSA level on all three of enforce, audit
and warn:

```yaml
pod-security.kubernetes.io/enforce: privileged
pod-security.kubernetes.io/audit: privileged
pod-security.kubernetes.io/warn: privileged
```

This is still required after dropping `privileged: true`. Baseline forbids host
namespaces, `hostPath` volumes, and adding any capability outside its default
set, and the agent needs all three: `hostPID`, the two read-only host mounts, and
`CAP_BPF` with `CAP_PERFMON`. Pod Security must be told so, or admission rejects
the DaemonSet outright on any cluster running `baseline` or `restricted`.

## RBAC

The ClusterRole is one rule:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
```

The informer narrows this to one node with a `spec.nodeName` field selector, but
RBAC cannot express that: field selectors are not authorization, so the grant is
necessarily cluster-wide on pods. There is deliberately nothing else. The node
name comes from the downward API rather than a `nodes` read, precisely so this
list stays one line long.

## Resource sizing

```yaml
resources:
  requests:
    cpu: 20m
    memory: 64Mi
  limits:
    memory: 128Mi
```

No CPU limit. The daemon's work is a ring buffer read and a periodic cgroup
scan, and throttling a diagnostic agent during the incident it exists to explain
is the wrong trade.

The 128Mi memory limit is what drives two design choices upstream of it. Pods
are trimmed before they enter the informer cache, dropping managed fields and
the last-applied annotation, which are routinely larger than the rest of the
object. Reports are capped by `--retain`, default 256. An OOM diagnostic that
OOMs on its own cache would be a poor advertisement.

If you raise `--history` or `--retain`, raise the limit with them.

## Probes

| Probe | Path | Meaning |
|---|---|---|
| Liveness | `/healthz` | The process is up |
| Readiness | `/readyz` | The detector is attached and history is being kept |

Readiness is deliberately not gated on the pod cache. See
[Correlation](correlation.md).

## The headless Service

Each agent holds only its own node's reports. A load-balanced VIP would answer a
question about one node's pod from a different node's daemon, which is worse
than not answering: it looks like a correct empty result.

The Service is therefore headless, and you query a specific agent:

```bash
kubectl -n oom-oracle port-forward pod/<agent-pod> 9090:9090
```

## The API is unauthenticated

Fine bound to a node. Not fine exposed. Anything beyond the node needs
authentication in front of it. See [Security](security.md).
