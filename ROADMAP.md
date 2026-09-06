# Roadmap

A statement of direction, not a promise of dates. Items move when evidence says
they should. Work is tracked on the
[issue tracker](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/issues).

## Where this is today

Released and installable. `v0.1.1` publishes signed CLI archives, a multi-arch
image, and a Helm chart listed on
[Artifact Hub](https://artifacthub.io/packages/helm/oom-oracle/oom-oracle).
Still pre-1.0, so the HTTP API and report JSON can change shape; every break is
written up with its migration in
[CHANGELOG.md](./CHANGELOG.md).

What works end to end, verified on a real kernel rather than fixtures:

- **Detection** by a CO-RE eBPF kprobe on `oom_kill_process`, with a cgroup
  polling detector as the fallback when BTF is absent or `bpf()` is denied.
- **Attribution** of a kill to a named process, its container-local PID, its
  resident memory, and the container's other processes as they stood at the
  moment of the kill. That listing is a snapshot rather than a survivor list:
  containerd sets `memory.oom.group=1`, so the kernel usually kills the whole
  container and nothing in it survives.
- **Correlation** from a cgroup path to `namespace/pod/container` through a
  node-scoped pod informer, so reports name workloads rather than UIDs.
- **Trajectory**, the memory samples leading up to the kill, plus the peak and
  the limit it was measured against.
- **Delivery** through an HTTP API and a terminal renderer.

## Next: reaching 1.0

1. **Settle the report schema.** `/v1/events` is what anything downstream builds
   on, and it has already broken once. 1.0 means it stops moving without a
   deprecation window.
2. **Prove the poller's victim attribution**, or say plainly that it is a guess.
   An inferred victim and a traced one differ by one boolean today, and the
   fallback is what runs on every node without BTF.

Shipped: runnable [example workloads](./examples/), a
[documentation site](https://ethan-kane-ops.github.io/k8s-pod-oom-oracle/), the
`oom-oracle watch` terminal dashboard, a release pipeline producing multi-arch
images and CLI archives, cosign-signed with an SBOM, and a Helm chart published
to `oci://ghcr.io/ethan-kane-ops/charts/oom-oracle` and listed on Artifact Hub.

## Known limitations, and what would fix them

- **The daemon runs as UID 0.** No longer `privileged`: it asks for `CAP_BPF`
  and `CAP_PERFMON` with everything else dropped. It cannot drop root as well.
  A non-root UID starts with an empty effective capability set however large its
  bounding set is, and populating it needs ambient capabilities that a pod spec
  cannot request, so `bpf()` would return `EPERM` and the daemon would fall back
  to polling without saying so.
- **The HTTP API is unauthenticated.** Fine bound to a node, not fine exposed.
  Anything beyond the node needs authentication in front of it.
- **The poller can miss a kill entirely.** It samples, so a container that
  allocates faster than the sample interval can die between two reads. This is
  the whole reason the eBPF detector exists, and why the poller is a fallback
  rather than an equal.
- **The victim's command line is best effort.** The kernel records a 15-character
  `comm`; anything longer means reading `/proc` while the process is being
  killed, and that race is lost often enough that the field is optional.
- **cgroup v1 has no real-kernel coverage.** The readers are unit tested against
  fixtures, but the e2e suite asserts v2, so nothing verifies v1 against a live
  hierarchy. Every bug this project has hit in anger was invisible to
  fixture-based tests.

## Not planned

- **Acting on an OOM**: no eviction, no limit adjustment, no restart policy. The
  probe is read-only and the project intends to keep it that way. A diagnostic
  that also intervenes is a much harder thing to trust on a node.
- **Replacing metrics pipelines.** Prometheus metrics and Kubernetes Events are
  worth emitting from the existing report hook, and may be. Storing and querying
  history is not this tool's job.

## Kernel and Kubernetes support

The eBPF detector needs Linux 5.8+ with BTF. Some fields degrade rather than
fail on older kernels: `memory.events.local` arrived in 5.13 and `memory.peak`
in 5.19. Below 5.8, or without BTF, the polling detector takes over.

Tested against the kernel and Kubernetes version pinned in CI. Older minors
likely work but are not verified.
