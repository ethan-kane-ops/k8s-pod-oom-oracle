#!/usr/bin/env bash
#
# Check that every flag the Helm chart renders is a flag the daemon accepts.
#
# This exists because the chart shipped `--history-size` while the binary's flag
# is `--history`. Nothing caught it: `helm lint` and `helm template` both pass,
# because a chart has no idea what the arguments mean. The result would have
# been a DaemonSet that installs cleanly and crash-loops on every node with
# "unknown flag", which is a bad way to meet a tool.
#
# The same applies to deploy/, which is what `kubectl apply -f deploy/` installs.
#
# Usage: hack/verify-chart-flags.sh
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

echo "▶ building the binary to read its real flags"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go build -o "$tmp/oom-oracle" ./cmd/oom-oracle

# Cobra prints one flag per line in the Flags: block, as "--name" or "-x, --name".
known="$("$tmp/oom-oracle" daemon --help | grep -oE '^[[:space:]]+(-[a-zA-Z], )?--[a-z-]+' | grep -oE '\-\-[a-z-]+' | sort -u)"

# Every long flag appearing in a rendered arg list, from both install paths.
rendered="$(
  {
    helm template verify-flags charts/oom-oracle
    helm template verify-flags charts/oom-oracle --set daemon.includeNonKubernetes=true
    cat deploy/20-daemonset.yaml
    cat examples/deployments/*.yaml
  } | grep -oE '^[[:space:]]*-[[:space:]]*--[a-z-]+' | grep -oE '\-\-[a-z-]+' | sort -u
)"

status=0
while read -r flag; do
  [ -n "$flag" ] || continue
  if ! grep -qx -- "$flag" <<<"$known"; then
    echo "✗ $flag is rendered but the daemon does not accept it" >&2
    status=1
  fi
done <<<"$rendered"

if [ "$status" -ne 0 ]; then
  echo >&2
  echo "The daemon accepts:" >&2
  sed 's/^/  /' <<<"$known" >&2
  exit 1
fi

echo "✓ every rendered flag exists on the daemon ($(wc -l <<<"$rendered" | tr -d ' ') checked)"
