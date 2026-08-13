// SPDX-License-Identifier: GPL-2.0
//go:build ignore

// Excluded from the Go build above: this is a BPF source file compiled by
// clang, not a cgo source file, and without the constraint the Go toolchain
// refuses to build the package at all.
//
// This file is GPL-2.0 because it calls GPL-only BPF helpers and the kernel
// refuses to load a program that claims otherwise. It is the only GPL-licensed
// file in this repository; the Go code that loads it is under the project's own
// licence, which is the same arrangement every CO-RE project ends up with.
//
// Traces OOM kills at the point the kernel decides on a victim.
//
// The hook is a kprobe on oom_kill_process(struct oom_control *oc, const char
// *message). Two alternatives were rejected:
//
//   - The oom/mark_victim tracepoint. Its layout is not a stable ABI: before
//     6.12 (and on distro kernels that backported the change earlier) it carried
//     only an int pid, afterwards a dozen fields. A program compiled against one
//     layout silently misreads the other.
//   - fentry/oom_kill_process. Cheaper and architecture-neutral, but the symbol
//     is static and absent from kernel BTF's FUNC list, so no trampoline can be
//     attached to it. Verified against a live 6.8 kernel.
//
// The kprobe fires on function entry, which is before SIGKILL is delivered, so
// oc->chosen still points at a live task and its mm is safe to read.

#include "oom_types.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

// oom_event is the wire format shared with the Go side. Field order and
// padding are load-bearing: internal/detector decodes these bytes directly.
// Any change here must be mirrored in rawEvent.
struct oom_event {
	__u64 timestamp_ns;
	__u64 memcg_id;
	__u64 task_cgroup_id;
	__u64 anon_rss_pages;
	__u64 file_rss_pages;
	__u64 shmem_rss_pages;
	__u64 total_vm_pages;
	__u64 limit_pages;
	__s64 badness_points;
	__u32 pid;
	__u32 tid;
	__u32 ppid;
	__u32 nspid;
	__s32 oom_score_adj;
	__u8 memcg_oom;
	__u8 pad[3];
	char comm[TASK_COMM_LEN];
};

// Forces bpf2go to emit a matching Go type, which the test suite compares
// against the hand-written decoder.
const struct oom_event *unused_oom_event __attribute__((unused));

// OOM kills are rare enough that a small ring is ample. Sizing it larger would
// only make a burst of kills, which is itself the pathology being diagnosed,
// consume more locked memory.
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 64 * 1024);
} events SEC(".maps");

// READ_RSS reads one resident-set counter, handling the 6.2 rewrite that
// replaced mm_struct's single mm_rss_stat with an array of percpu_counter.
//
// bpf_core_type_exists() resolves at load time, so the branch for the layout
// this kernel does not use is pruned by the verifier before it can execute. The
// relocations inside it are never applied, which is what makes one object file
// work on both sides of the change.
#define READ_RSS(mm, out, IDX)                                                 \
	do {                                                                   \
		__s64 __v = 0;                                                 \
		if (bpf_core_type_exists(struct mm_rss_stat)) {                \
			struct mm_struct___pre62 *__old = (void *)(mm);        \
			bpf_core_read(&__v, sizeof(__v),                       \
				      &__old->rss_stat.count[IDX]);            \
		} else {                                                       \
			bpf_core_read(&__v, sizeof(__v),                       \
				      &(mm)->rss_stat[IDX].count);             \
		}                                                              \
		(out) = __v < 0 ? 0 : (__u64)__v;                              \
	} while (0)

// ns_pid returns the victim's PID in its innermost namespace, which is the
// number a developer sees inside the container.
//
// pid->numbers is a flexible array indexed by namespace level, so the element
// cannot be named at compile time and BPF_CORE_READ cannot reach it. The offset
// of the array is relocated; the stride is sizeof(struct upid), which is an int
// and a pointer on every 64-bit kernel.
static __always_inline __u32 ns_pid(struct task_struct *task)
{
	struct pid *thread_pid = BPF_CORE_READ(task, thread_pid);
	if (!thread_pid)
		return 0;

	unsigned int level = BPF_CORE_READ(thread_pid, level);
	// A process nested deeper than this is not something the daemon can
	// report on usefully, and the bound is what lets the verifier accept
	// the computed offset.
	if (level > 32)
		return 0;

	unsigned long offset = bpf_core_field_offset(struct pid, numbers) +
			       (unsigned long)level * sizeof(struct upid);

	__u32 nr = 0;
	bpf_probe_read_kernel(&nr, sizeof(nr), (void *)thread_pid + offset);
	return nr;
}

SEC("kprobe/oom_kill_process")
int trace_oom_kill_process(void *ctx)
{
	struct oom_control *oc = (struct oom_control *)OOM_PARM1(ctx);
	if (!oc)
		return 0;

	struct task_struct *chosen = BPF_CORE_READ(oc, chosen);
	// out_of_memory() uses (void *)-1 to mean "a victim is already being
	// reaped". It does not call this function in that state, but the
	// sentinel is cheap to rule out and dereferencing it would not be.
	if (!chosen || (unsigned long)chosen == ~0UL)
		return 0;

	struct oom_event *event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
	if (!event)
		return 0;

	__builtin_memset(event, 0, sizeof(*event));

	event->timestamp_ns = bpf_ktime_get_ns();
	event->limit_pages = BPF_CORE_READ(oc, totalpages);
	event->badness_points = BPF_CORE_READ(oc, chosen_points);

	event->tid = BPF_CORE_READ(chosen, pid);
	event->pid = BPF_CORE_READ(chosen, tgid);
	event->ppid = BPF_CORE_READ(chosen, real_parent, tgid);
	event->nspid = ns_pid(chosen);
	event->oom_score_adj = BPF_CORE_READ(chosen, signal, oom_score_adj);
	bpf_core_read_str(&event->comm, sizeof(event->comm), &chosen->comm);

	// A memcg OOM names the cgroup whose limit was breached, which is the
	// container. A global OOM leaves this NULL: the node ran out of memory
	// and the victim merely had the worst badness score.
	struct mem_cgroup *memcg = BPF_CORE_READ(oc, memcg);
	if (memcg) {
		event->memcg_oom = 1;
		event->memcg_id = BPF_CORE_READ(memcg, css.cgroup, kn, id);
	}
	// Recorded even for a memcg OOM: the victim can sit in a descendant of
	// the cgroup that hit its limit.
	event->task_cgroup_id = BPF_CORE_READ(chosen, cgroups, dfl_cgrp, kn, id);

	struct mm_struct *mm = BPF_CORE_READ(chosen, mm);
	if (mm) {
		event->total_vm_pages = BPF_CORE_READ(mm, total_vm);
		READ_RSS(mm, event->anon_rss_pages, MM_ANONPAGES);
		READ_RSS(mm, event->file_rss_pages, MM_FILEPAGES);
		READ_RSS(mm, event->shmem_rss_pages, MM_SHMEMPAGES);
	}

	bpf_ringbuf_submit(event, 0);
	return 0;
}
