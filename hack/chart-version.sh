#!/usr/bin/env bash
#
# Set every version inside the chart to the one being released.
#
# Chart.yaml carries three: `version`, `appVersion`, and the image tag in the
# artifacthub.io/images annotation. The Release workflow refuses to publish when
# any of them disagrees with the git tag, and it checks that *after* the image
# job has already pushed, so a forgotten bump leaves a half-published release.
#
# `just release` calls this so the bump cannot be forgotten. It was previously a
# step in docs/releasing.md asking for a prior commit, which is the kind of
# instruction that works until the one release nobody re-reads it for.
#
# Usage: hack/chart-version.sh <vX.Y.Z> [Chart.yaml]
set -euo pipefail

version="${1:?usage: chart-version.sh <vX.Y.Z> [chart]}"
chart="${2:-charts/oom-oracle/Chart.yaml}"

# Chart versions are SemVer with no leading v, unlike the git tag.
want="${version#v}"
case "$want" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "not a version: $version" >&2; exit 1 ;;
esac

# In place via a temp file rather than `sed -i`, whose syntax differs between
# BSD and GNU. This has to run on a maintainer's Mac and on a runner.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

WANT="$want" awk '
  BEGIN { want = ENVIRON["WANT"] }
  /^version:[[:space:]]/    { print "version: " want; next }
  /^appVersion:[[:space:]]/ { print "appVersion: \"" want "\""; next }
  /image: ghcr\.io\/.*k8s-pod-oom-oracle:/ {
    sub(/:[^:]*$/, ":" want)
    print
    next
  }
  { print }
' "$chart" > "$tmp"

# Prove the rewrite hit all three before overwriting. A silently missed one is
# a release that fails in CI after the image has already been pushed.
for check in \
  "^version: ${want}\$" \
  "^appVersion: \"${want}\"\$" \
  "image: ghcr.io/.*k8s-pod-oom-oracle:${want}\$"
do
  if ! grep -qE "$check" "$tmp"; then
    echo "chart-version.sh did not set: $check" >&2
    exit 1
  fi
done

cat "$tmp" > "$chart"
echo "▶ chart pinned to ${want}"
