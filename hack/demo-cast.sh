#!/usr/bin/env bash
# The scenario behind the asciinema recording on ethankane.net.
#
# Unlike hack/demo.tape, which replays a captured run through hack/demo/serve.py
# so the README GIF is byte-stable, this drives a real kind cluster. The kernel
# does the kill, the daemon traces it with eBPF, and every number on screen was
# measured during the take. Nothing here is replayed and nothing is faked.
#
# That is also why it is a separate script. The GIF wants determinism; the site
# recording wants provenance, and those are different jobs.
#
#   just cast-setup   # cluster, agent, workload image — not recorded
#   just cast         # this script, under asciinema rec
#
# The only cluster it will touch is the local kind one named by CLUSTER. It
# refuses to run against anything else, because the recording is published and a
# real context name has no business on a public site.
set -euo pipefail

CLUSTER="${CLUSTER:-oom-oracle-e2e}"
CONTEXT="kind-${CLUSTER}"
NS_AGENT="oom-oracle"
POD="oom-gradual-leak"
BIN="${BIN:-./bin/oom-oracle}"

# Refuse to record against anything that is not the local kind cluster. A
# recording that leaked a real context name would have to be pulled from the
# site and rebuilt, so this is checked before a single frame exists.
case "$CONTEXT" in
  kind-*) ;;
  *) echo "refusing to record against non-kind context: $CONTEXT" >&2; exit 1 ;;
esac

k() { kubectl --context "$CONTEXT" "$@"; }

# Typing, so the cast reads as a session rather than a wall of output. The
# delay is per character; asciinema records real time and `--idle-time-limit`
# caps the gaps where the cluster, not the typist, is the slow part.
type_line() {
  local line="$1" i
  printf '\033[38;5;108m$\033[0m '
  for ((i = 0; i < ${#line}; i++)); do
    printf '%s' "${line:i:1}"
    sleep 0.022
  done
  printf '\n'
}

# Every command shown is the command that runs. `eval` on the same string the
# viewer just watched being typed is the whole point: there is no second,
# tidier version of it hiding in the script.
run() {
  type_line "$1"
  eval "$1"
  echo
}

note() {
  printf '\033[38;5;245m# %s\033[0m\n' "$1"
  sleep 1.4
}

cleanup() {
  [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

clear

note "A container is about to exceed its memory limit."
note "This is a local kind cluster. The kill is real."
echo

run "k apply -f examples/workloads/gradual-leak.yaml"

note "It leaks a megabyte at a time against a 128Mi limit."
note "The kernel will step in in about a minute."
echo

# The real wait. asciinema records it as it happens; --idle-time-limit trims the
# dead air on playback without pretending it was faster than it was.
type_line "k wait --for=jsonpath='{.status.phase}'=Failed pod/$POD --timeout=180s"
k wait --for=jsonpath='{.status.phase}'=Failed "pod/$POD" --timeout=180s
echo

note "Now ask Kubernetes what happened."
echo

run "k get pod $POD -o jsonpath='{.status.containerStatuses[0].state.terminated}' | jq ."

note "OOMKilled, exit code 137. That is the whole story it has."
note "Not which process died. Not what the climb looked like."
sleep 1.2
echo

note "OOM Oracle traced the same kill in the kernel."
echo

run "k -n $NS_AGENT port-forward daemonset/oom-oracle 9090:9090 >/dev/null 2>&1 &"
PF_PID=$!
# Wait for the forward rather than sleeping at it, so a slow one does not
# produce a recording of a connection error.
until curl -sf http://127.0.0.1:9090/v1/events >/dev/null 2>&1; do sleep 0.3; done

run "$BIN inspect $POD"

note "Named the process, and showed the curve that got it there."
sleep 2
