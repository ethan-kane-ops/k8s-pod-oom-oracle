// Package version exposes build metadata injected at link time.
package version

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Injected via -ldflags "-X github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version.version=..."
var (
	version = ""
	commit  = ""
	date    = ""
)

const (
	unknownVersion = "dev"
	unknownValue   = "unknown"
	shortSHALen    = 7
)

// Info describes the build that produced the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// vcsStamps holds the version-control metadata the Go toolchain embeds when a
// binary is built from a checkout without explicit ldflags.
type vcsStamps struct {
	revision string
	time     string
	modified bool
}

// Get resolves build metadata. Link-time values are authoritative; the
// toolchain's VCS stamps only fill in fields the linker left empty.
func Get() Info {
	info := Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	var mainVersion string
	var stamps vcsStamps
	if bi, ok := debug.ReadBuildInfo(); ok {
		mainVersion = bi.Main.Version
		stamps = readVCSStamps(bi.Settings)
	}

	return withDefaults(applyBuildInfo(info, mainVersion, stamps))
}

// readVCSStamps extracts VCS settings without depending on their ordering.
// debug.BuildSetting is sorted by key, so "vcs.modified" is observed before
// "vcs.revision"; reading them into a struct first avoids that trap.
func readVCSStamps(settings []debug.BuildSetting) vcsStamps {
	var stamps vcsStamps
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			stamps.revision = setting.Value
		case "vcs.time":
			stamps.time = setting.Value
		case "vcs.modified":
			stamps.modified = setting.Value == "true"
		}
	}
	return stamps
}

// applyBuildInfo fills empty fields from build info, leaving link-time values
// untouched.
func applyBuildInfo(info Info, mainVersion string, stamps vcsStamps) Info {
	if info.Version == "" && mainVersion != "" && mainVersion != "(devel)" {
		info.Version = mainVersion
	}
	if info.Commit == "" && stamps.revision != "" {
		info.Commit = shortCommit(stamps.revision)
		if stamps.modified {
			info.Commit += "-dirty"
		}
	}
	if info.Date == "" && stamps.time != "" {
		info.Date = stamps.time
	}
	return info
}

// withDefaults substitutes placeholders so no field is ever rendered empty.
func withDefaults(info Info) Info {
	if info.Version == "" {
		info.Version = unknownVersion
	}
	if info.Commit == "" {
		info.Commit = unknownValue
	}
	if info.Date == "" {
		info.Date = unknownValue
	}
	return info
}

// String renders Info as aligned, human-readable lines.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "version:  %s\n", i.Version)
	fmt.Fprintf(&b, "commit:   %s\n", i.Commit)
	fmt.Fprintf(&b, "built:    %s\n", i.Date)
	fmt.Fprintf(&b, "go:       %s\n", i.GoVersion)
	fmt.Fprintf(&b, "platform: %s\n", i.Platform)
	return b.String()
}

// JSON renders Info as indented JSON with a trailing newline.
func (i Info) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling version info: %w", err)
	}
	return append(b, '\n'), nil
}

// shortCommit truncates a full git SHA to the conventional 7 characters.
func shortCommit(sha string) string {
	if len(sha) <= shortSHALen {
		return sha
	}
	return sha[:shortSHALen]
}
