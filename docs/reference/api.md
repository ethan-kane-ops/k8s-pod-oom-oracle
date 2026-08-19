# API Reference

The daemon serves HTTP on `--listen`, `:9090` by default.

!!! warning "Unauthenticated"
    There is no authentication. It is intended to be reachable from the node it
    runs on, not from the cluster network. See [Security](../security.md).

!!! note "Pre-release"
    No version has been tagged. The routes and the report JSON may still change
    shape, and have: the report's process listing was named `hogs` before it
    became `processes`.

## Routes

| Route | Returns |
|---|---|
| `GET /healthz` | `ok` once the process is up |
| `GET /readyz` | `ready` once the detector is attached and history is being kept |
| `GET /v1/status` | Operational snapshot |
| `GET /v1/events` | All reports, newest first |
| `GET /v1/events/{id}` | One report, or 404 |

### `GET /v1/events`

Query parameters, all optional and combinable:

| Parameter | Effect |
|---|---|
| `namespace` | Only reports in this namespace |
| `pod` | Only reports for this pod name |
| `container` | Only reports for this container name |
| `limit` | At most this many reports, newest first |

A non-numeric or negative `limit` is a `400`. An unknown id on
`/v1/events/{id}` is a `404`. Both return a JSON error object.

Responses are served with `Content-Type: application/json` and
`X-Content-Type-Options: nosniff`, because reports carry values read from the
node's filesystem (cgroup paths, process command lines) and a browser must never
interpret those as markup.

## Status schema

```json
{
  "detector": "ebpf",
  "cgroupVersion": "v2",
  "ready": true,
  "reports": 3,
  "skipped": 128,
  "unattributed": 0,
  "trackedCgroups": 47,
  "uptimeSeconds": 3812.4,
  "version": "v0.1.0",
  "node": "worker-1",
  "podCacheSynced": true,
  "podsTracked": 10
}
```

| Field | Type | Meaning |
|---|---|---|
| `detector` | string | Active detection method: `ebpf`, `poller`, or `fake` |
| `cgroupVersion` | string | Hierarchy layout in use: `v1`, `v2`, or `unknown` |
| `ready` | bool | Mirrors `/readyz` |
| `reports` | uint | Post-mortems produced since start |
| `skipped` | uint | Kills discarded as belonging to no Kubernetes container |
| `unattributed` | uint | The subset of `skipped` that came from inside the kubepods tree |
| `trackedCgroups` | int | Containers that currently have sampled history |
| `uptimeSeconds` | float | How long the daemon has been running |
| `version` | string | Build the daemon was compiled from |
| `node` | string | Node whose pods are watched. Omitted when correlation is off |
| `podCacheSynced` | bool | Whether the informer finished its initial list |
| `podsTracked` | int | Pods on this node the cache holds |

!!! tip "`unattributed` is the one to alert on"
    `skipped` climbs on any busy node, because the probes see every kill on the
    machine. It is not a fault and cannot be alerted on. `unattributed` is a real
    Kubernetes OOM kill the daemon could not place and therefore never reported.
    It should stay at zero.

## Report schema

```json
{
  "id": "20260813T081522Z-0001",
  "time": "2026-08-13T08:15:22Z",
  "identity": {
    "podUID": "3f0e2b6c-1a2b-4c3d-9e8f-0a1b2c3d4e5f",
    "containerID": "9a8b7c6d5e4f...",
    "kind": "container",
    "qos": "Burstable",
    "driver": "systemd",
    "runtime": "containerd",
    "cgroupPath": "/kubepods.slice/.../cri-containerd-9a8b....scope",
    "namespace": "default",
    "podName": "payment-api-6d5f78",
    "containerName": "web-server",
    "image": "payment:v1.2.0",
    "resolved": true
  },
  "victim": {
    "pid": 28145,
    "nsPid": 17,
    "comm": "node",
    "cmdline": ["node", "./dist/garbage-collector.js"],
    "rssBytes": 119537664,
    "inferred": false,
    "known": true
  },
  "source": "ebpf",
  "killCount": 1,
  "limitBytes": 536870912,
  "peakBytes": 536870912,
  "trajectory": [
    {
      "time": "2026-08-13T08:14:22Z",
      "usedBytes": 432013312,
      "limitBytes": 536870912,
      "ratio": 0.8,
      "pressureFull": 0.0
    }
  ],
  "processes": [
    {
      "pid": 28102,
      "nsPid": 1,
      "comm": "node",
      "cmdline": ["node", "./dist/server.js"],
      "rssBytes": 408944640
    }
  ],
  "groupKill": true,
  "trend": {
    "bytesPerSecond": 1048576,
    "rSquared": 0.97,
    "samples": 5,
    "window": 60000000000,
    "timeToLimit": 0,
    "projected": true
  }
}
```

### Top level

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Unique within one daemon lifetime, sorts chronologically |
| `time` | RFC 3339 | When the kill was observed. For the poller, up to one interval late |
| `identity` | object | The pod and container the kill belongs to |
| `victim` | object | The process the kernel killed |
| `source` | string | `ebpf`, `poller`, or `fake` |
| `killCount` | uint | The container's cumulative OOM kill count |
| `limitBytes` | uint | The memory ceiling that was breached. Zero when uncapped |
| `peakBytes` | uint | High-water mark, from `memory.peak` where the kernel exposes it |
| `trajectory` | array | Memory history leading up to the kill, oldest first |
| `processes` | array | The container's process list at report time, heaviest first, victim removed |
| `groupKill` | bool | Whether the cgroup is killed as an indivisible unit |
| `trend` | object | Growth analysis over the trajectory |

### `identity`

| Field | Type | Meaning |
|---|---|---|
| `podUID` | string | Pod UID in canonical dashed form |
| `containerID` | string | Container ID without its runtime prefix. Empty for a pod scope |
| `kind` | string | `container` or `pod` |
| `qos` | string | `Guaranteed`, `Burstable`, `BestEffort`, or `Unknown` |
| `driver` | string | `systemd`, `cgroupfs`, or `unknown` |
| `runtime` | string | `containerd`, `cri-o`, `docker`, or `unknown` |
| `cgroupPath` | string | The path the scope was parsed from |
| `namespace` | string | From the API server. Empty when unresolved |
| `podName` | string | From the API server. Empty when unresolved |
| `containerName` | string | From the API server. Empty when unresolved |
| `image` | string | From the API server. Empty when unresolved |
| `resolved` | bool | Whether the cluster fields were populated |

`kind: "pod"` means the kill was charged to the pod slice rather than to any one
container, which is what a memory-backed `emptyDir` produces. There is no
container ID in that case. See [Correlation](../correlation.md).

### `victim`

| Field | Type | Meaning |
|---|---|---|
| `pid` | int | Host-namespace process ID |
| `nsPid` | int | PID as seen inside the container, which is what application logs show |
| `comm` | string | The kernel's 15-character executable name |
| `cmdline` | array | Full argument vector. Omitted when the `/proc` read lost the race |
| `rssBytes` | uint | Resident memory at, or shortly before, death |
| `inferred` | bool | True means deduced from which process vanished, not named by the kernel |
| `known` | bool | False means no victim was identified at all |

### `trajectory[]`

| Field | Type | Meaning |
|---|---|---|
| `time` | RFC 3339 | When the reading was taken |
| `usedBytes` | uint | Memory in use |
| `limitBytes` | uint | The ceiling in force. Zero when uncapped |
| `ratio` | float | `usedBytes / limitBytes`, in `[0,1]` |
| `pressureFull` | float | The `memory.pressure` full ten-second average |

### `processes[]`

| Field | Type | Meaning |
|---|---|---|
| `pid` | int | Host-namespace process ID |
| `nsPid` | int | PID as seen inside the container |
| `comm` | string | The kernel's 15-character executable name |
| `cmdline` | array | Full argument vector. Omitted when unreadable |
| `rssBytes` | uint | Resident set size |

!!! danger "This is a snapshot, not a survivor list"
    When `groupKill` is false, the two are the same thing. When it is true the
    kernel is killing every process in the cgroup, so this is whatever was still
    readable mid-teardown: entries are missing, and resident sizes are already
    collapsing towards zero.

    What it never contains is the victim itself.

### `groupKill`

Reflects `memory.oom.group` on the cgroup the report is attributed to. containerd
sets it on the container scope, so on almost every current cluster it is true and
an OOM takes the whole container down.

!!! warning "False means not observed"
    False is reported both when the container genuinely survives and when the
    daemon could not tell, because under group kill the cgroup is frequently torn
    down before it can be read. Do not read false as a promise that anything
    survived.

### `trend`

| Field | Type | Meaning |
|---|---|---|
| `bytesPerSecond` | float | Fitted growth rate. Negative means memory is falling |
| `rSquared` | float | Goodness of fit in `[0,1]`. A flat line yields 1 |
| `samples` | int | How many readings informed the fit |
| `window` | int | Nanoseconds the fit covers |
| `timeToLimit` | int | Nanoseconds until usage reaches the limit |
| `projected` | bool | Whether `timeToLimit` was computed at all |

`timeToLimit` is meaningful only when `projected` is true. It is false when
memory is flat or falling, the cgroup is uncapped, or the window is too short to
fit a line.
