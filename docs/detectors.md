# Detectors

Detection sits behind an interface with two implementations. `--detector` picks
one; the default `auto` tries eBPF and falls back to polling, saying so in the
logs.

| | `ebpf` | `poller` |
|---|---|---|
| Source | kprobe on `oom_kill_process` | `memory.events.local` counters |
| Victim | named by the kernel as it chose | deduced from which process vanished |
| Memory at death | exact, read from the victim's `mm` | from the last snapshot, up to one interval stale |
| Command line | read from `/proc` before `SIGKILL` lands | often unavailable |
| Needs | BTF, `CAP_BPF`/`CAP_SYS_ADMIN`, kernel ≥ 5.8 | readable `cgroupfs`, kernel ≥ 5.13 |

## The same kill, both ways

A container whose forked child is killed while the container itself survives:

```
ebpf     victim=tail  cmdline=[tail /dev/zero]  rss=511.0MiB  inferred=false
poller   victim=tail  cmdline=<none>            rss=0         inferred=true
```

The poller found the right process by name but had only a snapshot taken before
it grew, so it reports no memory and flags the answer as a guess. Reports carry
`source` and `victim.inferred` precisely so a reader can tell which of these
they are looking at.

## The eBPF detector

A kprobe on `oom_kill_process` fires as the kernel selects a victim, before
`SIGKILL` is delivered. That timing is what makes the rest possible: the victim
is still readable in `/proc` for the moment it takes to enrich the event, which
is where the full command line comes from. The kernel itself records only the
15-character `comm`.

The probe reads kernel structs through CO-RE, so one compiled object works
across kernel versions that moved the fields, including the 6.2 rewrite of
`mm_struct.rss_stat`.

!!! note "Why a kprobe and not the tracepoint"
    It attaches to a kprobe rather than the `oom/mark_victim` tracepoint because
    that tracepoint's layout is not a stable ABI.

## The polling detector

The poller reads `memory.events.local` counters and notices when the OOM kill
count rises between two passes. It then infers the victim by comparing the
current process list against the previous snapshot: whichever process was there
before and is gone now.

Where several processes vanished in the same interval, the one holding the most
memory is chosen, because that is who the kernel's badness heuristic targets.
That is a guess, and the returned victim is marked `inferred` so a report says
so rather than presenting it as fact.

## A known gap in the poller on Kubernetes

!!! danger "The poller can miss a Kubernetes kill entirely"
    It detects a kill by noticing a counter rise between two passes, which needs
    the cgroup to still exist on the second one. When a container is killed
    outright the kubelet tears its cgroup down within milliseconds, and the kill
    goes unseen.

The eBPF detector has no such dependency: it observes the kill as the kernel
makes it. Prefer eBPF on Kubernetes and treat the poller as the degraded mode it
is.

The poller can also miss a kill for a second reason: it samples, so a container
that allocates faster than the sample interval can die between two reads. This
is the whole reason the eBPF detector exists.

## Choosing

`--detector auto` is the default and the right choice for most deployments. It
attaches eBPF where it can and degrades where it cannot, logging which it got.

Use `--detector ebpf` when a silent degradation would be worse than a crash. It
fails loudly rather than falling back, which is what you want in CI or on a node
pool you control and have verified.

Use `--detector poller` only when you know the probe cannot attach and you want
to skip the attempt. See [Run Without BTF](guides/no-btf.md).
