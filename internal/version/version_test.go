package version

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// TestReadVCSStampsIsOrderIndependent guards a real trap: debug.BuildSetting is
// sorted by key, so "vcs.modified" always arrives before "vcs.revision".
// Reading them in a single pass and mutating as you go silently breaks.
func TestReadVCSStampsIsOrderIndependent(t *testing.T) {
	t.Parallel()

	// Alphabetical order, as the toolchain actually emits it.
	alphabetical := []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.modified", Value: "true"},
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.time", Value: "2026-08-13T00:00:00Z"},
	}
	reversed := make([]debug.BuildSetting, len(alphabetical))
	for i, s := range alphabetical {
		reversed[len(alphabetical)-1-i] = s
	}

	want := vcsStamps{revision: "0123456789abcdef", time: "2026-08-13T00:00:00Z", modified: true}

	for name, settings := range map[string][]debug.BuildSetting{
		"alphabetical": alphabetical,
		"reversed":     reversed,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := readVCSStamps(settings); got != want {
				t.Errorf("readVCSStamps() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestApplyBuildInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		info        Info
		mainVersion string
		stamps      vcsStamps
		want        Info
	}{
		{
			name:        "link-time values are authoritative",
			info:        Info{Version: "1.2.3", Commit: "deadbee", Date: "2026-01-01T00:00:00Z"},
			mainVersion: "v9.9.9",
			stamps:      vcsStamps{revision: "0123456789abcdef", time: "2026-08-13T00:00:00Z", modified: true},
			want:        Info{Version: "1.2.3", Commit: "deadbee", Date: "2026-01-01T00:00:00Z"},
		},
		{
			name:        "falls back to vcs stamps when unset",
			mainVersion: "v9.9.9",
			stamps:      vcsStamps{revision: "0123456789abcdef", time: "2026-08-13T00:00:00Z"},
			want:        Info{Version: "v9.9.9", Commit: "0123456", Date: "2026-08-13T00:00:00Z"},
		},
		{
			name:   "dirty tree marks the fallback commit",
			stamps: vcsStamps{revision: "0123456789abcdef", modified: true},
			want:   Info{Commit: "0123456-dirty"},
		},
		{
			name:        "devel main version is ignored",
			mainVersion: "(devel)",
			want:        Info{},
		},
		{
			name:   "dirty flag alone does not invent a commit",
			stamps: vcsStamps{modified: true},
			want:   Info{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := applyBuildInfo(tt.info, tt.mainVersion, tt.stamps); got != tt.want {
				t.Errorf("applyBuildInfo() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWithDefaults(t *testing.T) {
	t.Parallel()

	got := withDefaults(Info{GoVersion: "go1.26.3", Platform: "linux/amd64"})
	want := Info{
		Version:   unknownVersion,
		Commit:    unknownValue,
		Date:      unknownValue,
		GoVersion: "go1.26.3",
		Platform:  "linux/amd64",
	}
	if got != want {
		t.Errorf("withDefaults() = %+v, want %+v", got, want)
	}

	populated := Info{Version: "1.0.0", Commit: "abc1234", Date: "2026-08-13T00:00:00Z"}
	if got := withDefaults(populated); got != populated {
		t.Errorf("withDefaults() overwrote populated fields: %+v", got)
	}
}

func TestShortCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sha  string
		want string
	}{
		{name: "full sha truncated", sha: "0123456789abcdef0123456789abcdef01234567", want: "0123456"},
		{name: "exactly seven kept", sha: "0123456", want: "0123456"},
		{name: "shorter than seven kept", sha: "abc", want: "abc"},
		{name: "empty", sha: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shortCommit(tt.sha); got != tt.want {
				t.Errorf("shortCommit(%q) = %q, want %q", tt.sha, got, tt.want)
			}
		})
	}
}

func TestGetNeverReturnsEmptyFields(t *testing.T) {
	t.Parallel()

	info := Get()

	fields := map[string]string{
		"Version":   info.Version,
		"Commit":    info.Commit,
		"Date":      info.Date,
		"GoVersion": info.GoVersion,
		"Platform":  info.Platform,
	}
	for name, value := range fields {
		if value == "" {
			t.Errorf("Get().%s is empty; every field must carry a placeholder", name)
		}
	}

	if !strings.Contains(info.Platform, "/") {
		t.Errorf("Get().Platform = %q, want GOOS/GOARCH form", info.Platform)
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()

	info := Info{
		Version:   "1.2.3",
		Commit:    "abc1234",
		Date:      "2026-08-13T00:00:00Z",
		GoVersion: "go1.26.3",
		Platform:  "linux/amd64",
	}

	got := info.String()

	for _, want := range []string{"1.2.3", "abc1234", "2026-08-13T00:00:00Z", "go1.26.3", "linux/amd64"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("String() must end with a newline")
	}
}

func TestInfoJSON(t *testing.T) {
	t.Parallel()

	info := Info{
		Version:   "1.2.3",
		Commit:    "abc1234",
		Date:      "2026-08-13T00:00:00Z",
		GoVersion: "go1.26.3",
		Platform:  "linux/amd64",
	}

	raw, err := info.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("JSON() must end with a newline")
	}

	var round Info
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshaling JSON() output: %v", err)
	}
	if round != info {
		t.Errorf("round-trip = %+v, want %+v", round, info)
	}
}
