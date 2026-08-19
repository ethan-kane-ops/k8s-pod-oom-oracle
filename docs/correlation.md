# Correlation

A cgroup path proves a pod UID, a container ID, and a QoS class. It cannot prove
a name. Turning `pode257c815_3e31_...` into
`oom-oracle-e2e/e2e-multi-process/app` takes the API server, which is what the
pod informer is for.

| | Without the informer | With it |
|---|---|---|
| Identity | pod UID, container ID, QoS | plus namespace, pod, container, image |
| Needs | nothing | `pods: get,list,watch` and a node name |

## What the path itself yields

Parsing a cgroup path produces a scope, which is what a report's identity is
built on even when the API server is unavailable.

| Field | Meaning |
|---|---|
| `podUID` | The pod's UID, in canonical dashed form |
| `containerID` | The runtime's container ID, without its runtime prefix. Empty for a pod scope |
| `kind` | `container` or `pod`, the level of the tree the path names |
| `qos` | The QoS tier encoded in the path |
| `driver` | The cgroup driver that produced the path |
| `runtime` | The container runtime that owns the container |
| `cgroupPath` | The path the scope was parsed from |

### Container scopes and pod scopes

A kill can be charged to a container's own cgroup or to the pod slice above it.
The second happens when memory is charged to the pod rather than to any one
container, which a memory-backed `emptyDir` does: its tmpfs pages belong to the
pod slice, and the pod's limit is the larger of its containers' sum and its
greediest init container.

A pod-level report carries `kind: "pod"` and no container ID, and the rendered
identity is marked `(pod-level)`. Without that marker the line is identical to a
container kill whose name could not be looked up, and the two want opposite
responses: one is a shared allocation to go and find, the other is stale pod
metadata to ignore.

!!! note "Why a pod scope needs a kubepods ancestor"
    A container segment is a long hex ID that pins a path down by itself. A pod
    segment is `pod` followed by anything, so without requiring a `kubepods`
    ancestor a host directory named `podman` or `podinfo` would parse as
    somebody's pod and misattribute a host kill.

## The pod informer

It is deliberately narrow:

- **Scoped to one node** by the field selector `spec.nodeName=$NODE_NAME`, taken
  from the downward API. A DaemonSet has no business watching every pod in the
  cluster, and there is no cluster-wide fallback: a daemon that could not learn
  its node name refuses to start the informer rather than quietly watching
  everything.
- **Keyed by pod UID**, because that is the only key a cgroup path offers.
- **Container IDs include the previous one.** After an OOM kill the container
  restarts with a new ID while the cgroup path recorded against the kill still
  names the dead one, so `lastState.terminated.containerID` is indexed too.
  Without it a restarted container resolves to a pod but not to a container.
- **Deleted pods stay resolvable for five minutes.** A container killed outright
  takes its pod with it, and under a controller the replacement arrives within
  seconds. Dropping the entry on the delete event would lose the identity at
  exactly the moment the tool exists to explain.
- **Pods are trimmed before caching**, dropping managed fields and the
  last-applied annotation, which are routinely larger than the rest of the
  object. The daemon ships with a 128Mi limit, and an OOM diagnostic that OOMs
  on its own cache would be a poor advertisement.

## The `--kubernetes` modes

| Value | Behaviour |
|---|---|
| `auto` (default) | Try the API server. On failure, degrade to UID-only correlation and log why |
| `on` | Refuse to start without the API server |
| `off` | Never contact the API server |

## Readiness is not gated on the cache

`/readyz` means the detector is attached and history is being kept. Coupling
that to control plane availability would get a perfectly good daemon restarted
during an API server blip, when it is still recording every kill on its node
correctly and only lacks the names.

`/v1/status` reports `node`, `podCacheSynced` and `podsTracked` instead, so the
cache state is observable without being a liveness concern. Reports produced
before the cache syncs identify pods by UID alone.

## The cost

client-go. It takes the stripped `linux/amd64` binary from 10.8MB to 48.4MB,
which is ordinary for Kubernetes tooling but not nothing. `--kubernetes off`
does not shrink the binary, only the traffic.
