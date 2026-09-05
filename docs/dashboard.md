# Dashboard

`oom-oracle watch` is a live terminal view of one node's kills.

```bash
kubectl -n oom-oracle port-forward pod/<agent-pod> 9090:9090
oom-oracle watch
```

It reads the daemon's HTTP API and needs no privileges of its own. Run it from a
laptop against a port-forward, not on the node.

## What is on screen

The header states what the daemon is and whether to trust what follows:

```
oom-oracle  http://127.0.0.1:9090
detector ebpf  node node-1  ready
reports 2  tracking 48  unattributed 0
```

`detector` is the one to read first. A `poller` badge says so in full, because an
inferred victim and a traced one are the same shape in every field but a boolean:

```
detector poller (victims are inferred, not traced)
```

`unattributed` is the counter worth alerting on: kills from inside the kubepods
tree that could not be placed against a pod. It is coloured when it moves.
`skipped` is deliberately absent, because it climbs on any busy node and means
nothing on its own. See [the API reference](reference/api.md#get-v1status).

The left pane lists reports newest first. The right pane renders the selected
one with the same function `oom-oracle inspect` prints, so the two cannot
disagree about what a report says.

## Keys

| Key | Does |
|---|---|
| `↑` `↓` or `k` `j` | Move the selection, or scroll the report when the detail pane has focus |
| `g` / `G` | First / last report |
| `tab` | Switch which pane has focus |
| `space` / `pgup` | Scroll the report by half a page |
| `r` | Refresh now |
| `q` / `esc` / `ctrl+c` | Quit |

## Layout

Below 164 columns the panes stop sitting side by side and `tab` switches between
them instead. The report is fixed-width text about 108 columns across, and half
of a narrower terminal wraps the trajectory chart into noise.

## When the daemon goes away

The reports already fetched stay on screen and the footer says the view is
stale:

```
↑↓ move  tab pane  r refresh  q quit  daemon unreachable: connection refused
```

This is deliberate. A node under memory pressure can take the daemon with it,
and that is exactly when the last few reports matter most. It keeps retrying at
the refresh interval; `r` retries immediately.

## Selection while kills arrive

The selection follows the report, not its position. Reports arrive newest first,
so a kill landing while you read an older one shifts every row down. Tracking the
index instead would move the selection to a different report at the one moment
someone is reading it.
