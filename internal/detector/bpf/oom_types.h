/* Minimal kernel type declarations for the OOM tracer.
 *
 * This replaces the ~4MB vmlinux.h that `bpftool btf dump` produces. Only the
 * fields the probe actually reads are declared, and every struct carries
 * __attribute__((preserve_access_index)) so clang emits CO-RE relocations for
 * them. libbpf then resolves each field against the running kernel's BTF at
 * load time, matching on the field *name*.
 *
 * That is what makes the layouts below irrelevant to correctness: they exist to
 * satisfy the compiler, not to describe any particular kernel. Declaring a
 * subset of a struct's fields, in the wrong order, with unrelated fields
 * omitted, is not merely allowed, it is the point. struct mm_struct is
 * __randomize_layout in the kernel, so its offsets are not even stable between
 * two builds of the same version.
 *
 * Every field name here was verified against a live kernel's BTF. A name that
 * does not exist in the target kernel makes the program fail to load with a
 * clear relocation error rather than reading the wrong memory.
 */

#ifndef __OOM_TYPES_H__
#define __OOM_TYPES_H__

/* UAPI headers, not kernel-internal ones. linux/types.h supplies the __u8..__u64
 * and __beNN typedefs that libbpf's bpf_helper_defs.h expects; linux/bpf.h
 * supplies enum bpf_map_type and the context structs it references. Both are
 * architecture-neutral, which the kernel-internal headers are not. */
#include <linux/bpf.h>
#include <linux/types.h>

typedef __s32 pid_t;

/* TASK_COMM_LEN. Fixed at 16 for the entire history of the field. */
#define TASK_COMM_LEN 16

/* Indices into mm_struct.rss_stat, from enum in include/linux/mm_types.h.
 * Stable since 3.x. */
#define MM_FILEPAGES 0
#define MM_ANONPAGES 1
#define MM_SWAPENTS 2
#define MM_SHMEMPAGES 3

#define __ctx_attr __attribute__((preserve_access_index))

struct kernfs_node {
	__u64 id;
} __ctx_attr;

struct cgroup {
	struct kernfs_node *kn;
} __ctx_attr;

struct cgroup_subsys_state {
	struct cgroup *cgroup;
} __ctx_attr;

struct mem_cgroup {
	struct cgroup_subsys_state css;
} __ctx_attr;

struct css_set {
	struct cgroup *dfl_cgrp;
} __ctx_attr;

struct upid {
	int nr;
	struct pid_namespace *ns;
} __ctx_attr;

struct pid {
	unsigned int level;
	struct upid numbers[1];
} __ctx_attr;

/* percpu_counter.count is the batched total. Per-CPU deltas not yet folded in
 * are ignored: they are bounded by a small batch size and irrelevant next to a
 * memory limit measured in megabytes. */
struct percpu_counter {
	__s64 count;
} __ctx_attr;

/* Pre-6.2 kernels keep RSS in a plain atomic array instead. Both flavours are
 * declared so the probe can pick one at load time; see rss_pages(). */
struct mm_rss_stat {
	__s64 count[4];
} __ctx_attr;

struct mm_struct {
	unsigned long total_vm;
	struct percpu_counter rss_stat[4];
} __ctx_attr;

struct mm_struct___pre62 {
	struct mm_rss_stat rss_stat;
} __ctx_attr;

struct signal_struct {
	short oom_score_adj;
} __ctx_attr;

struct task_struct {
	pid_t pid;
	pid_t tgid;
	struct task_struct *real_parent;
	struct pid *thread_pid;
	struct mm_struct *mm;
	struct signal_struct *signal;
	struct css_set *cgroups;
	char comm[TASK_COMM_LEN];
} __ctx_attr;

/* mm/oom_kill.c. chosen is the victim; memcg is NULL for a global OOM.
 * chosen_points is the badness score the kernel used to pick it. */
struct oom_control {
	struct mem_cgroup *memcg;
	unsigned long totalpages;
	struct task_struct *chosen;
	long chosen_points;
} __ctx_attr;

/* Register layout for reading the first argument of a kprobe.
 *
 * libbpf's PT_REGS_PARM1 would do this, but it reads struct pt_regs out of
 * vmlinux.h, whose definition is architecture-specific: x86_64 names the
 * registers, arm64 exposes an array. Declaring both here keeps a single set of
 * committed BPF objects buildable for both architectures from one header.
 *
 * A kprobe's context is PTR_TO_CTX, so the verifier permits these reads
 * directly rather than through bpf_probe_read_kernel.
 */
#if defined(__TARGET_ARCH_arm64)

struct oom_pt_regs {
	__u64 regs[31];
};
#define OOM_PARM1(ctx) (((struct oom_pt_regs *)(ctx))->regs[0])

#elif defined(__TARGET_ARCH_x86)

struct oom_pt_regs {
	__u64 r15, r14, r13, r12, bp, bx, r11, r10, r9, r8, ax, cx, dx, si, di;
};
#define OOM_PARM1(ctx) (((struct oom_pt_regs *)(ctx))->di)

#else
#error "unsupported architecture: define __TARGET_ARCH_arm64 or __TARGET_ARCH_x86"
#endif

#endif /* __OOM_TYPES_H__ */
