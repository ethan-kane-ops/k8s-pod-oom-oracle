package procfs

import (
	"reflect"
	"testing"
)

func TestParseStatus(t *testing.T) {
	t.Parallel()

	const containerised = `Name:	node
Umask:	0022
State:	S (sleeping)
Tgid:	28145
Ngid:	0
Pid:	28145
PPid:	28102
TracerPid:	0
Uid:	0	0	0	0
NSpid:	28145	17	1
VmPeak:	  1249072 kB
VmSize:	  1216304 kB
VmRSS:	   116736 kB
Threads:	11
`

	tests := []struct {
		name  string
		input string
		want  Status
	}{
		{
			name:  "containerised process reports its innermost namespace pid",
			input: containerised,
			want: Status{
				Name: "node", State: "S", PID: 28145, PPID: 28102, NSPid: 1,
				RSSBytes: 116736 * 1024, VMSizeBytes: 1216304 * 1024,
			},
		},
		{
			name:  "host process without an NSpid line falls back to its pid",
			input: "Name:\tbash\nState:\tR (running)\nPid:\t1234\nPPid:\t1200\nVmRSS:\t   4096 kB\nVmSize:\t  20480 kB\n",
			want: Status{
				Name: "bash", State: "R", PID: 1234, PPID: 1200, NSPid: 1234,
				RSSBytes: 4096 * 1024, VMSizeBytes: 20480 * 1024,
			},
		},
		{
			name:  "kernel thread has no memory fields",
			input: "Name:\tkworker/0:1\nState:\tI (idle)\nPid:\t42\nPPid:\t2\n",
			want:  Status{Name: "kworker/0:1", State: "I", PID: 42, PPID: 2, NSPid: 42},
		},
		{
			name:  "unknown fields are ignored",
			input: "Name:\tx\nPid:\t1\nSomeFutureField:\t999\nCoreDumping:\t0\n",
			want:  Status{Name: "x", PID: 1, NSPid: 1},
		},
		{
			name:  "lines without a colon are skipped",
			input: "Name:\tx\ngarbage line\nPid:\t7\n",
			want:  Status{Name: "x", PID: 7, NSPid: 7},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseStatus([]byte(tt.input))
			if err != nil {
				t.Fatalf("ParseStatus() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseStatus() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestParseStatusErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "non-numeric pid", input: "Pid:\tnotanumber\n"},
		{name: "non-numeric ppid", input: "PPid:\tnotanumber\n"},
		{name: "malformed VmRSS", input: "VmRSS:\tlots kB\n"},
		{name: "malformed NSpid", input: "NSpid:\t123\tbroken\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseStatus([]byte(tt.input)); err == nil {
				t.Errorf("ParseStatus(%q) = nil error, want error", tt.input)
			}
		})
	}
}

func TestParseCmdline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "multiple arguments",
			input: "node\x00./dist/garbage-collector.js\x00--max-old-space-size=512\x00",
			want:  []string{"node", "./dist/garbage-collector.js", "--max-old-space-size=512"},
		},
		{name: "single argument", input: "sleep\x00", want: []string{"sleep"}},
		{name: "no trailing NUL", input: "sleep", want: []string{"sleep"}},
		{name: "kernel thread has none", input: "", want: nil},
		{name: "only NULs", input: "\x00\x00", want: nil},
		{
			name:  "empty argument preserved between NULs",
			input: "sh\x00-c\x00\x00tail\x00",
			want:  []string{"sh", "-c", "", "tail"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseCmdline([]byte(tt.input)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseCmdline(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseCgroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "unified single line",
			input: "0::/docker/6dd161dc81cc713b668eef2d5e13bbeaeacbd18f939f7f26cd965a6c2e2f0397\n",
			want:  "/docker/6dd161dc81cc713b668eef2d5e13bbeaeacbd18f939f7f26cd965a6c2e2f0397",
		},
		{
			name:  "unified at the root",
			input: "0::/\n",
			want:  "/",
		},
		{
			name: "legacy hierarchy picks the memory controller",
			input: "12:pids:/kubepods/burstable/pod-abc\n" +
				"9:memory:/kubepods/burstable/pod-abc/container\n" +
				"3:cpu,cpuacct:/kubepods/burstable/pod-abc\n",
			want: "/kubepods/burstable/pod-abc/container",
		},
		{
			name: "memory listed alongside other controllers",
			input: "5:cpu,memory,cpuacct:/kubepods/besteffort/pod-xyz/container\n" +
				"2:pids:/other\n",
			want: "/kubepods/besteffort/pod-xyz/container",
		},
		{
			name: "unified wins on a hybrid host",
			input: "9:memory:/legacy/path\n" +
				"0::/unified/path\n",
			want: "/unified/path",
		},
		{
			name:  "path containing a colon is preserved",
			input: "0::/kubepods/pod-abc/weird:name\n",
			want:  "/kubepods/pod-abc/weird:name",
		},
		{name: "no memory controller", input: "12:pids:/some/path\n", wantErr: true},
		{name: "empty", input: "", wantErr: true},
		{name: "malformed lines only", input: "garbage\nmore garbage\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCgroup([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseCgroup(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCgroup() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseCgroup() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseNamespaceInode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "pid namespace", input: "pid:[4026531836]", want: 4026531836},
		{name: "mount namespace", input: "mnt:[4026532715]", want: 4026532715},
		{name: "no brackets", input: "pid:4026531836", wantErr: true},
		{name: "reversed brackets", input: "pid:]4026531836[", wantErr: true},
		{name: "non-numeric inode", input: "pid:[abc]", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseNamespaceInode(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseNamespaceInode(%q) = %d, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNamespaceInode() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseNamespaceInode(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
