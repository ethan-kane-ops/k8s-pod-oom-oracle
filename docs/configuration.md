# Configuration

## `oom-oracle daemon`

Watches this node and serves the results. Intended to run as a DaemonSet.

| Flag | Default | Purpose |
|---|---|---|
| `--detector` | `auto` | `auto`, `ebpf`, or `poller`. `auto` falls back; `ebpf` fails loudly instead |
| `--cgroup-root` | `/sys/fs/cgroup` | Path to the cgroup hierarchy |
| `--proc-root` | `/proc` | Path to the proc filesystem |
| `--cgroup-prefix` | `/` | Limit watching to one cgroup subtree |
| `--listen` | `:9090` | HTTP API address |
| `--sample-interval` | `1s` | How often memory is sampled |
| `--poll-interval` | `500ms` | How often the poller checks for kills |
| `--history` | `60` | Memory samples retained per container |
| `--retain` | `256` | Reports retained in memory |
| `--include-non-kubernetes` | `false` | Also report kills outside the `kubepods` tree |
| `--kubernetes` | `auto` | Pod-name resolution: `auto`, `on`, `off` |
| `--node-name` | `$NODE_NAME` | Node whose pods to watch |
| `--kubeconfig` | in-cluster | Credentials to use instead of the in-cluster ones |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--log-format` | `text` | `text` or `json` |

### Choosing intervals

`--sample-interval` sets the resolution of the trajectory. A container that
allocates faster than one interval will not show a climb, which is why
`peakBytes` is read from the kernel's `memory.peak` rather than from the
samples: it stays honest when the curve has no shape.

`--history` multiplied by `--sample-interval` is how far back a report can see.
The defaults give sixty seconds. Raising history costs memory per tracked
container, and the daemon ships with a 128Mi limit.

`--poll-interval` matters only to the polling detector. Shortening it narrows
but never closes the window in which a torn-down cgroup hides a kill. See
[Detectors](detectors.md).

### Scoping what is watched

`--cgroup-prefix` limits sampling to one subtree, normally `/kubepods.slice`.
Narrowing it reduces work on a node running much besides Kubernetes.

`--include-non-kubernetes` keeps kills from cgroups outside the kubepods tree.
It is off by default: the probes see every kill on the node, and attributing a
host service crash to a pod is worse than missing it.

## `oom-oracle inspect [pod]`

Renders post-mortems from a running daemon. With no argument, every recorded
kill is listed newest first. The pod argument accepts a bare name or
`pod/<name>`.

| Flag | Default | Purpose |
|---|---|---|
| `--daemon` | `http://127.0.0.1:9090` | Base URL of the daemon to query |
| `-n`, `--namespace` | | Filter by namespace |
| `-c`, `--container` | | Filter by container name |
| `--limit` | all | Maximum reports to render, newest first |
| `-o`, `--output` | `text` | `text` or `json` |

## `oom-oracle version`

Build metadata, as `text` or `json`.

## Environment

| Variable | Used for |
|---|---|
| `NODE_NAME` | The default for `--node-name`, normally set from the downward API |
