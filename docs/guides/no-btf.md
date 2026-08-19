# Run Without BTF

The eBPF detector needs BTF, the kernel's own type information, because the
probe reads kernel structs through CO-RE. Without it the probe cannot relocate
its field accesses and will not load.

## Check whether you have it

```bash
ls -l /sys/kernel/btf/vmlinux
```

A file there means the kernel was built with `CONFIG_DEBUG_INFO_BTF=y`. Most
distribution kernels from 2021 onwards have it. If it is missing, the kernel
predates it or was built without it.

## What happens by default

`--detector auto` tries eBPF, fails to attach, logs the reason, and falls back
to polling. The daemon keeps running and keeps producing reports.

Confirm which you got:

```bash
curl -s localhost:9090/v1/status | jq .detector
```

```json
"poller"
```

## Skip the attempt

If you know the probe cannot attach, `--detector poller` goes straight to
polling and does not log an attach failure on every restart.

```yaml
args:
  - daemon
  - --detector=poller
```

## What you lose

| | `ebpf` | `poller` |
|---|---|---|
| Victim | named by the kernel as it chose | deduced from which process vanished |
| Memory at death | exact, read from the victim's `mm` | from the last snapshot, up to one interval stale |
| Command line | read from `/proc` before `SIGKILL` lands | often unavailable |

Every report carries `source` and `victim.inferred`, so a consumer can tell a
traced victim from a deduced one without knowing how the daemon was configured.

!!! danger "The poller can miss Kubernetes kills entirely"
    It detects a kill by noticing a counter rise between two passes, which needs
    the cgroup to still exist on the second one. When a container is killed
    outright the kubelet tears its cgroup down within milliseconds, and the kill
    goes unseen.

    This is not tuning you can fix. Shortening `--poll-interval` narrows the
    window but never closes it. Treat the poller as the degraded mode it is.

## Fail loudly instead

On a node pool you control and have verified, a silent degradation is worse than
a crash. `--detector ebpf` refuses to fall back:

```yaml
args:
  - daemon
  - --detector=ebpf
```

The pod then crashloops with the attach error rather than quietly recording
worse data, which is what you want in CI.

## Other reasons the probe will not attach

BTF is only one requirement. See [Troubleshooting](../troubleshooting.md) for
capability loss on a non-root UID, which produces the same fallback with a
different cause.
