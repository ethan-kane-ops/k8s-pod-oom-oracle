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
# --check runs the annotation guard alone and writes nothing, so `just release`
# can fail on a stale changelog before git-cliff has rewritten CHANGELOG.md.
# Failing after that left a dirty tree the operator had to unpick by hand.
#
# Usage: hack/chart-version.sh [--check] <vX.Y.Z> [Chart.yaml]
set -euo pipefail

check_only=false
if [ "${1:-}" = "--check" ]; then
  check_only=true
  shift
fi

version="${1:?usage: chart-version.sh [--check] <vX.Y.Z> [chart]}"
chart="${2:-charts/oom-oracle/Chart.yaml}"

# Chart versions are SemVer with no leading v, unlike the git tag.
want="${version#v}"
case "$want" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "not a version: $version" >&2; exit 1 ;;
esac

# Artifact Hub renders artifacthub.io/changes as the listing's release notes, and
# nothing downstream can tell that they describe the previous release: the chart
# version is correct, the image tag is correct, and the notes are a version out.
# Every entry links to the release it describes, so requiring those links to name
# this tag is a mechanical check on prose that is otherwise unverifiable.
#
# Run before anything is rewritten, so a stale block aborts `just release` with
# the working tree untouched and no tag pushed.
stale=$(grep -oE 'releases/tag/v[0-9]+\.[0-9]+\.[0-9]+' "$chart" | grep -v "releases/tag/v${want}$" || true)
if [ -n "$stale" ]; then
  echo "artifacthub.io/changes still points at a previous release:" >&2
  printf '  %s\n' $stale >&2
  echo "" >&2
  echo "Artifact Hub would publish ${want}'s listing with the previous release's" >&2
  echo "notes. Rewrite the annotation in $chart to describe ${want}, linking to:" >&2
  echo "  https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/releases/tag/v${want}" >&2
  exit 1
fi
if ! grep -q "artifacthub.io/changes:" "$chart"; then
  echo "$chart has no artifacthub.io/changes annotation" >&2
  echo "the Artifact Hub listing would report no changelog for ${want}" >&2
  exit 1
fi

if [ "$check_only" = true ]; then
  echo "▶ chart changelog describes ${want}"
  exit 0
fi

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
