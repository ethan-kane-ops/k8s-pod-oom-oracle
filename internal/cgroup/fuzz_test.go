package cgroup

import "testing"

// These parsers read files the kernel writes. That is not a guarantee of shape:
// the format has changed between kernel versions before (memory.events.local
// arrived in 5.13, memory.peak in 5.19), the root cgroup omits rows others
// have, and a containerised daemon may be pointed at a bind mount holding
// anything at all. A parser that panics takes the whole daemon with it, so the
// property under test is that these return an error rather than crash.

func FuzzParseUint(f *testing.F) {
	f.Add([]byte("0"))
	f.Add([]byte("134217728\n"))
	f.Add([]byte("max\n"))
	f.Add([]byte(""))
	f.Add([]byte("18446744073709551615"))
	f.Add([]byte("-1"))
	f.Add([]byte("  42  \n\n"))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParseUint(data)
	})
}

func FuzzParseLimit(f *testing.F) {
	f.Add([]byte("max"))
	f.Add([]byte("max\n"))
	f.Add([]byte("536870912\n"))
	f.Add([]byte("9223372036854771712\n")) // the v1 uncapped sentinel
	f.Add([]byte(""))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParseLimit(data)
	})
}

func FuzzParseKeyValue(f *testing.F) {
	f.Add([]byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 1\n"))
	f.Add([]byte("anon 4096\nfile 8192\nkernel 1024\n"))
	f.Add([]byte("key"))             // no value
	f.Add([]byte("key value extra")) // too many fields
	f.Add([]byte("key notanumber\n"))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := ParseKeyValue(data)
		if err == nil && got == nil {
			t.Error("ParseKeyValue returned a nil map and no error; callers index it directly")
		}
	})
}

func FuzzParsePSI(f *testing.F) {
	f.Add([]byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
		"full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"))
	// The root cgroup legitimately omits the full row.
	f.Add([]byte("some avg10=1.50 avg60=0.75 avg300=0.10 total=12345\n"))
	f.Add([]byte("some avg10=notafloat avg60=0 avg300=0 total=0\n"))
	f.Add([]byte("some avg10=0.00\n"))
	f.Add([]byte("unknown avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"))
	f.Add([]byte("some=1\n"))
	f.Add([]byte(""))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = ParsePSI(data)
	})
}

// FuzzParsePIDList guards the one parser that reads a file the kernel rewrites
// while it is being read. A torn or interleaved cgroup.procs must never panic
// and must never invent a PID: every entry returned is used to read /proc and
// then to decide which process the kernel killed.
func FuzzParsePIDList(f *testing.F) {
	f.Add([]byte("28102\n28145\n"))
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte("0\n-1\n99999999999999999999\n"))
	f.Add([]byte("28102\x0028145\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		pids, err := ParsePIDList(data)
		if err != nil {
			return
		}
		for _, pid := range pids {
			if pid <= 0 {
				t.Errorf("ParsePIDList(%q) returned pid %d; a non-positive pid names "+
					"no process and would be read from /proc as one", data, pid)
			}
		}
	})
}
