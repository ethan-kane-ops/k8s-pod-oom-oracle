# Diagnose a Multi-Process Container

This is the case the project exists for. A container runs several processes, one
of them takes the memory, and Kubernetes tells you only that the container was
`OOMKilled`.

## The workload

`examples/workloads/multi-process-survivor.yaml` runs a shell with several
`sleep` workers beside one process that balloons.

```bash
kubectl apply -f examples/workloads/multi-process-survivor.yaml
```

## What Kubernetes says

```bash
kubectl get pod oom-multi-process
```

Under containerd: `OOMKilled`, exit 137. It cannot say which of the processes
took the memory, and every worker in the container looks identical from the
control plane.

## What the report says

```bash
oom-oracle inspect oom-multi-process
```

```
VICTIM PROCESS:
  PID:             28145 (in container: 17)
  Command:         tail /dev/zero
  Exit code:       137 (OOM)
  Memory at death: 511.0MiB
  Confidence:      traced in the kernel at the moment of the kill.
```

`in container: 17` is the number that matches the application's own logs. The
host PID appears nowhere inside the container and is useless for correlating
against anything the workload wrote.

`Confidence` distinguishes a victim the kernel named from one deduced afterwards.
See [Detectors](../detectors.md).

## The runtime split that changes the outcome

`memory.oom.group` on the container cgroup decides whether the kernel kills one
process or all of them.

| Runtime | `memory.oom.group` | What happens |
|---|---|---|
| containerd, CRI-O | 1 | Every process in the container dies. The pod goes `Failed`, reason `OOMKilled`, exit 137 |
| Docker, Moby | 0 | Only the victim dies. The container keeps running, the pod stays `Running`, restart count stays 0, and Kubernetes reports nothing at all |

The Docker shim was removed in Kubernetes 1.24, so containerd or CRI-O is what
almost every current cluster runs. The survivor case is the uncommon one.

### Check which you are on

```bash
find /sys/fs/cgroup -path '*cri-containerd*' -name memory.oom.group \
  -exec sh -c 'echo "$1: $(cat "$1")"' _ {} \;
```

Or read it from the report: `groupKill` is the same value, read by the daemon.

!!! warning "Do not use pod phase as evidence of survival"
    `kubectl get pod` reads `Running` for about a second after the container is
    already dead, because the pod phase lags the kill. Check
    `.status.containerStatuses[0].state.terminated.reason` instead, which is
    populated first.

## Reading the process listing

```
PROCESSES IN CONTAINER AFTER THE KILL:
  1. node ./dist/server.js (PID 28102) - 390.0MiB
  2. node ./dist/worker.js (PID 28160) - 8.0MiB
```

Under group kill this is a teardown snapshot rather than a survivor list.
Entries will be missing and resident sizes read low or zero, because the kernel
is already reclaiming them. It is still useful for seeing what else was in the
container, and it never contains the victim.

Under `memory.oom.group=0` it is a genuine survivor list.

## When the victim is unknown

```
victim.known: false
```

The kill happened in a container whose processes were never sampled, so there is
nothing to name. This is most likely on the polling detector against a container
that appeared and died inside one sample window.
