package procfs

import (
	"bytes"
	"strings"
	"testing"
)

// /proc content is the least trustworthy input this daemon reads. It is written
// by the kernel but describes processes the daemon does not control, it changes
// shape between kernel versions, and it is read while the process it describes
// is being killed, so a truncated or partially-written file is normal rather
// than exceptional.

func FuzzParseStatus(f *testing.F) {
	f.Add([]byte("Name:\ttail\nPid:\t28145\nPPid:\t28102\nState:\tR (running)\nVmRSS:\t114688 kB\n"))
	f.Add([]byte("Name:\tapp\nPid:\t1\nNSpid:\t1\nVmRSS:\t0 kB\n"))
	// Container-local PIDs appear as a tab-separated list, innermost last.
	f.Add([]byte("Name:\tworker\nPid:\t28145\nNSpid:\t28145\t17\n"))
	f.Add([]byte("VmRSS:\tnotanumber kB\n"))
	f.Add([]byte("VmRSS:\t114688\n")) // no unit
	f.Add([]byte("Pid:\t-1\n"))
	f.Add([]byte("Name:"))
	f.Add([]byte(""))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParseStatus(data)
	})
}

func FuzzParseCmdline(f *testing.F) {
	f.Add([]byte("tail\x00/dev/zero\x00"))
	f.Add([]byte("node\x00./dist/server.js\x00"))
	// A kernel thread has an empty cmdline, which is not an error.
	f.Add([]byte(""))
	f.Add([]byte("\x00\x00\x00"))
	f.Add([]byte("sh\x00-c\x00sleep 3\ntail /dev/zero\n\x00"))
	f.Add([]byte("no-null-terminator"))

	f.Fuzz(func(t *testing.T, data []byte) {
		got := ParseCmdline(data)

		// A nil result is correct and documented: kernel threads have an empty
		// cmdline. The invariant that does hold is that the split is lossless,
		// because the report prints these arguments back to a reader.
		trimmed := bytes.TrimRight(data, "\x00")
		if len(trimmed) == 0 {
			if len(got) != 0 {
				t.Errorf("ParseCmdline(%q) = %q, want no arguments", data, got)
			}
			return
		}

		if rejoined := strings.Join(got, "\x00"); rejoined != string(trimmed) {
			t.Errorf("ParseCmdline(%q) rejoined to %q, want %q", data, rejoined, trimmed)
		}
	})
}

func FuzzParseCgroup(f *testing.F) {
	f.Add([]byte("0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope\n"))
	f.Add([]byte("12:memory:/docker/abc123\n11:cpu:/docker/abc123\n"))
	// A reader inside a private cgroup namespace sees a relative path, which
	// matches nothing under /sys/fs/cgroup until it is normalised.
	f.Add([]byte("0::/../../system.slice/containerd.service\n"))
	f.Add([]byte("0::/\n"))
	f.Add([]byte("garbage without colons\n"))
	f.Add([]byte("0:\n"))
	f.Add([]byte("::::\n"))
	f.Add([]byte(""))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParseCgroup(data)
	})
}

func FuzzParseNamespaceInode(f *testing.F) {
	f.Add("pid:[4026531836]")
	f.Add("mnt:[4026531840]")
	f.Add("pid:[]")
	f.Add("pid:[notanumber]")
	f.Add("pid:4026531836")
	f.Add("")

	f.Fuzz(func(_ *testing.T, target string) {
		_, _ = ParseNamespaceInode(target)
	})
}
