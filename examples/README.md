# Examples

Runnable manifests that make the daemon produce real reports. Every "expected
report" in these files was copied from an actual run on kind against containerd
v2.3.1, runc 1.4.2, kernel 6.8. Where a claim depends on your runtime, the file
says so and tells you how to check.

Install the daemon first:

```bash
kubectl apply -f deploy/
```

## Workloads

| File | What it shows |
|---|---|
| [`workloads/single-process-killed.yaml`](workloads/single-process-killed.yaml) | The simplest case, and why `peakBytes` and `trajectory` disagree |
| [`workloads/worker-pool.yaml`](workloads/worker-pool.yaml) | Which of five processes took the memory |
| [`workloads/multi-process-survivor.yaml`](workloads/multi-process-survivor.yaml) | The container surviving its child, and the runtime setting that decides whether it can |
| [`workloads/jvm-heap-overrun.yaml`](workloads/jvm-heap-overrun.yaml) | `-Xmx` above the container limit, killed before any application code runs |

Apply one, wait a few seconds, then read the report:

```bash
kubectl apply -f examples/workloads/single-process-killed.yaml

DAEMON=$(kubectl -n oom-oracle get pods -l app.kubernetes.io/name=oom-oracle \
  -o jsonpath='{.items[0].metadata.name}')
kubectl get --raw "/api/v1/namespaces/oom-oracle/pods/$DAEMON:9090/proxy/v1/events" | jq .
```

The daemon's image is distroless and has no shell, so reach it through the API
server proxy as above, or with `kubectl port-forward`. `kubectl exec` will not
work.

The first two workloads are also what the end-to-end suite runs. It reads these
exact files, so a sample that stops producing the report its header describes
fails CI rather than quietly becoming wrong.

## Deployment variants

Patches against the base DaemonSet. Apply the base first, then one patch.

| File | Trade |
|---|---|
| [`deployments/poller-only.yaml`](deployments/poller-only.yaml) | No eBPF requirement, at the cost of missing fast kills entirely |
| [`deployments/no-api-server.yaml`](deployments/no-api-server.yaml) | No cluster-wide pod read, at the cost of UID-only reports |

```bash
kubectl -n oom-oracle patch daemonset oom-oracle \
  --patch-file examples/deployments/poller-only.yaml
```

## Three things these samples will teach you the hard way

**A flat trajectory does not mean an idle container.** The sampler reads once a
second. `tail /dev/zero` reaches a 128 MiB limit in far less than that, so every
sample says 2 MiB and the peak says 128 MiB. The peak comes from the kernel's
`memory.peak`, which does not depend on anyone watching. For any allocation
faster than the sample interval, the peak is the only honest number. Trajectory
earns its place on slow leaks, not on runaway ones.

**Your runtime decides whether a container can survive an OOM kill.**
`memory.oom.group` on the container cgroup is 1 under containerd, which kills
every process in the container, and 0 under Docker, which kills only the process
the kernel selected. The same manifest therefore behaves differently on
different clusters. `multi-process-survivor.yaml` covers both and shows how to
check which you have.

**`victim.cmdline` is usually null.** Recovering it means reading `/proc` while
the process is being killed, and that race is normally lost; it was lost in
every run behind these files. Identify a victim by `comm` and `nsPid`. `nsPid`
is the PID as seen inside the container, which is the one your application's own
logs will show; the host `pid` beside it matches nothing you can see from
inside.

## Cleaning up

```bash
kubectl delete pod -l app.kubernetes.io/name=oom-oracle-example
```

Every sample sets `restartPolicy: Never`, so a killed pod stays in `Failed` for
inspection instead of restarting into a fresh OOM every few seconds.
