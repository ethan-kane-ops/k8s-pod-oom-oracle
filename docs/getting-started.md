# Getting Started

!!! warning "No published artifacts yet"
    No container image or Helm chart has been published. Until the release
    pipeline lands, the supported path is to build the image yourself and load
    it into your cluster.

The daemon needs the host's `/sys/fs/cgroup` and `/proc` mounted read-only,
`hostPID`, and enough privilege to load a BPF program. The `deploy/` directory
contains a DaemonSet configured that way. See [Deployment](deployment.md) for
what each grant is for.

## On kind, end to end

One command creates the cluster, builds the image, loads it, and rolls out the
DaemonSet:

```bash
just e2e-deploy
```

## With Helm

```bash
helm install oom-oracle oci://ghcr.io/ethan-kane-ops/charts/oom-oracle \
  --namespace oom-oracle --create-namespace

kubectl label namespace oom-oracle \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged
```

The label is required even though the agent is not `privileged: true`:
`hostPID`, `hostPath` volumes and the two capabilities it adds are each outside
the `baseline` level on their own. Without it admission rejects the pods and the
DaemonSet reports no event explaining why.

The chart's values are documented in
[its README](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/charts/oom-oracle/README.md).

## On any cluster

```bash
# 1. Build and push the image to a registry your nodes can reach
docker build -t <your-registry>/oom-oracle:dev .
docker push <your-registry>/oom-oracle:dev

# 2. Point the DaemonSet at it and apply
kubectl apply -f deploy/
kubectl -n oom-oracle set image daemonset/oom-oracle oom-oracle=<your-registry>/oom-oracle:dev
kubectl -n oom-oracle rollout status daemonset/oom-oracle
```

## Confirm the agent is watching

```bash
kubectl -n oom-oracle port-forward pod/<agent-pod> 9090:9090
curl -s localhost:9090/v1/status | jq
```

```json
{
  "detector": "ebpf",
  "cgroupVersion": "v2",
  "ready": true,
  "reports": 3,
  "skipped": 128,
  "unattributed": 0,
  "trackedCgroups": 47,
  "node": "worker-1",
  "podCacheSynced": true,
  "podsTracked": 10
}
```

`detector: "poller"` means the eBPF probe did not attach. The daemon logs the
reason at startup, and [Troubleshooting](troubleshooting.md) covers the usual
causes.

!!! tip "Which counter to alert on"
    `skipped` counts kills that belonged to no Kubernetes container. It climbs
    on any busy node, because the probes see every kill on the machine, so it is
    not a fault and cannot be alerted on.

    `unattributed` is the subset that came from inside the kubepods tree,
    meaning a real Kubernetes OOM kill the daemon could not place and therefore
    never reported. That one should stay at zero, and it is the field to alert
    on.

## Trigger a kill

The repository ships runnable workloads under `examples/workloads/`, each with
an expected report copied from a real run.

```bash
kubectl apply -f examples/workloads/single-process-killed.yaml
```

| Workload | What it demonstrates |
|---|---|
| `single-process-killed.yaml` | The simplest case: one process, one limit, one kill |
| `multi-process-survivor.yaml` | Which of several processes the kernel chose, and the `memory.oom.group` split |
| `worker-pool.yaml` | A pool of identical workers where only the report can say which one died |
| `jvm-heap-overrun.yaml` | A heap that outgrows the container limit rather than the JVM's own |
| `shared-memory-pod-level.yaml` | A memory-backed `emptyDir` charged to the pod slice rather than any container |

## Read the reports

The Service is headless on purpose: each agent holds only its own node's
reports, so a load-balanced VIP would answer a question about one node's pod
from a different node's daemon. Query a specific agent.

```bash
oom-oracle inspect                      # every recorded kill, newest first
oom-oracle inspect payment-api-6d5f78   # one pod
oom-oracle inspect -n default -o json   # filtered, machine-readable
```

`-o json` emits the same report as a machine-readable object. See the
[API Reference](reference/api.md) for its schema.

## Next steps

- [Detectors](detectors.md) explains why you want eBPF rather than the poller on
  Kubernetes.
- [Configuration](configuration.md) lists every flag.
- [Diagnose a Multi-Process Container](guides/multi-process.md) walks through
  the case the tool exists for.
