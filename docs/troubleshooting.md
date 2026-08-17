# Troubleshooting

## The detector fell back to the poller

```bash
curl -s localhost:9090/v1/status | jq .detector
```

```json
"poller"
```

The daemon logs the reason at startup. The three usual causes:

### BTF is missing

```bash
ls -l /sys/kernel/btf/vmlinux
```

No file means the kernel was built without `CONFIG_DEBUG_INFO_BTF=y`. The probe
reads kernel structs through CO-RE and cannot relocate without it. See
[Run Without BTF](guides/no-btf.md).

### Capabilities were lost on a non-root UID

This one is silent and easy to miss. A non-root process starts with an **empty
effective capability set** regardless of how permissive its bounding set is, so
`bpf()` returns `EPERM` even under `privileged: true`.

The distroless base image runs as non-root, so the DaemonSet sets `runAsUser: 0`
explicitly. If you have templated that away, this is your cause.

```yaml
securityContext:
  privileged: true
  runAsUser: 0
```

### The kernel is older than 5.8

The eBPF detector needs 5.8 or newer. Some fields degrade rather than fail on
older kernels: `memory.events.local` arrived in 5.13 and `memory.peak` in 5.19.
Below 5.8, or without BTF, polling is the only option.

## No reports appear at all

### Check the daemon is watching

```bash
curl -s localhost:9090/v1/status | jq '{ready, trackedCgroups, detector}'
```

`trackedCgroups: 0` means the sampler is finding nothing. Check `--cgroup-root`
points at the host's hierarchy and `--cgroup-prefix` is not scoped to a subtree
that does not exist.

### Check you are asking the right agent

Each agent holds only its own node's reports. A pod on `worker-2` produces a
report on the `worker-2` agent and nowhere else. The Service is headless
precisely so a VIP cannot answer from the wrong node.

```bash
kubectl -n oom-oracle get pods -o wide
```

### The poller missed the kill

If `detector` is `poller` and a container was killed outright, the kill may
simply have been invisible: the kubelet tears the cgroup down within
milliseconds, and the poller needs it to still exist on its next pass. This is a
known gap, not a misconfiguration. See [Detectors](detectors.md).

## Reports exist but name UIDs instead of pods

```json
{ "identity": { "resolved": false, "podUID": "3f0e2b6c-..." } }
```

The pod informer has not resolved the UID. Check:

```bash
curl -s localhost:9090/v1/status | jq '{node, podCacheSynced, podsTracked}'
```

| Symptom | Cause |
|---|---|
| `node` is empty | `NODE_NAME` was not set. The informer refuses to start rather than watch every pod in the cluster |
| `podCacheSynced: false` | The initial list has not finished, or RBAC denies `pods: list` |
| `podsTracked: 0` with a node set | The field selector matches nothing. Check `NODE_NAME` matches the actual node name |

Reports produced before the cache syncs identify pods by UID alone, and are not
backfilled.

## QoS reads `Unknown` for every pod

The QoS tier is parsed out of the cgroup path. On a kubelet configured with its
own cgroup root, systemd flattens the parent slice into every child's name, so
paths look like `kubelet-kubepods-burstable-pod<uid>.slice` rather than starting
with `kubepods`.

This was a real bug: prefix matching reported every Guaranteed pod on such a
node as `Unknown`. Matching is now by containment. If you still see it, the path
shape is one the parser does not recognise, and the `cgroupPath` field in the
report is what to open an issue with.

## Kills are reported for things that are not pods

`skipped` counts them and they are dropped by default. The probes see every kill
on the node, including host services, and attributing a host service crash to a
pod is worse than missing it.

If you want them, `--include-non-kubernetes` keeps them, with `cgroupPath` as
the only identity.

## `unattributed` is climbing

This is a defect, not noise. It counts kills from **inside** the kubepods tree
that the daemon could not place, meaning a real Kubernetes OOM kill that never
became a report. The daemon logs each one at warn level with its cgroup path.

Open an issue with that path.

## cgroup v1

The readers are unit tested against fixtures, but the e2e suite asserts v2, so
nothing verifies v1 against a live hierarchy. Every bug this project has hit in
anger was invisible to fixture-based tests. Treat v1 support as unverified and
report what you find.

## Getting help

[SUPPORT.md](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/SUPPORT.md)
covers where to ask and what to include. For anything involving a
misattributed or missing report, include the report JSON or the cgroup path from
the warn log: it is the one piece of information that makes the problem
reproducible.
