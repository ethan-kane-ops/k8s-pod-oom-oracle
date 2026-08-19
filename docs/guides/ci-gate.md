# Gate a Pipeline on a Report

A load test that passes while quietly OOM-killing a worker is a load test that
lied. The daemon's API is machine-readable so a pipeline can fail on that.

## The shape of the check

Run the workload, then ask the daemon whether it killed anything.

```bash
#!/usr/bin/env bash
set -euo pipefail

DAEMON=${DAEMON:-http://127.0.0.1:9090}
NAMESPACE=${NAMESPACE:-default}

reports=$(curl -sf "$DAEMON/v1/events?namespace=$NAMESPACE")

count=$(jq 'length' <<<"$reports")
if [ "$count" -gt 0 ]; then
  echo "✗ $count OOM kill(s) during this run"
  jq -r '.[] | "  \(.identity.podName)/\(.identity.containerName): \(.victim.comm) held \(.victim.rssBytes) of \(.limitBytes)"' <<<"$reports"
  exit 1
fi

echo "✓ no OOM kills"
```

## Reaching the daemon

Each agent holds only its own node's reports, so there is no single endpoint for
the cluster. In a pipeline the usual options are:

```bash
# Port-forward one agent, when the workload is pinned to a known node
kubectl -n oom-oracle port-forward pod/<agent-pod> 9090:9090 &

# Or go through the API server's pod proxy, which needs no forwarding
kubectl get --raw \
  "/api/v1/namespaces/oom-oracle/pods/<agent-pod>:9090/proxy/v1/events"
```

The e2e suite in this repository uses the pod proxy, because managing a
background `port-forward` process and racing on a local port for every call is
worse than one extra API call.

## Filtering to the run

`/v1/events` accepts `namespace`, `pod`, `container`, and `limit`. Deploy the
workload under test into its own namespace and filter on it, so an unrelated
kill elsewhere on the node does not fail your build.

```bash
curl -sf "$DAEMON/v1/events?namespace=perf-run-$BUILD_ID"
```

## Failing on the right thing

A report existing is not always a failure. Some suites deliberately provoke an
OOM to prove a limit works. Gate on the fields instead:

```bash
# Fail only when the victim was not the process we expected to die
jq -e '[.[] | select(.victim.comm != "stress-ng")] | length == 0' <<<"$reports"
```

```bash
# Fail when anything died under its own limit, which means the limit was wrong
jq -e '[.[] | select(.victim.rssBytes < (.limitBytes / 2))] | length == 0' <<<"$reports"
```

## Checking the daemon was actually watching

An empty result means "no kills" only if the daemon was running and attached.
Otherwise it means nothing at all.

```bash
status=$(curl -sf "$DAEMON/v1/status")
jq -e '.ready and .reports >= 0' <<<"$status" >/dev/null || {
  echo "daemon not ready; the empty result proves nothing"; exit 1
}
```

Also assert `unattributed` did not move during the run. It counts Kubernetes OOM
kills the daemon saw but could not place, which is the one case where a clean
report is actively misleading:

```bash
jq -e '.unattributed == 0' <<<"$status"
```

## Detector confidence

If the pipeline depends on the victim's identity being right, require the eBPF
detector rather than accepting a deduced answer:

```bash
jq -e '.detector == "ebpf"' <<<"$status"
```

Or per report, `victim.inferred == false`. See [Detectors](../detectors.md).
