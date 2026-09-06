package hack

import (
	"regexp"
	"strings"
	"testing"
)

var (
	releaseTag   = regexp.MustCompile(`releases/tag/v\d+\.\d+\.\d+`)
	chartVersion = regexp.MustCompile(`(?m)^version: (\d+\.\d+\.\d+)$`)
)

// The three places Chart.yaml states a version, in the shape the real file
// uses, plus the artifacthub.io/changes block whose release links pin the
// listing's notes to one tag.
const chartTemplate = `apiVersion: v2
name: oom-oracle
description: Node agent that explains Kubernetes OOM kills
type: application
version: 0.1.0
appVersion: "0.1.0"
home: https://github.com/ethan-kane-ops/k8s-pod-oom-oracle
annotations:
  artifacthub.io/license: Apache-2.0
  artifacthub.io/images: |
    - name: oom-oracle
      image: ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:0.1.0
  artifacthub.io/changes: |
    - kind: fixed
      description: Something a commit subject could not have said
      links:
        - name: Release notes
          url: https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/releases/tag/vLINKED
`

// chartFixture is the template with its changes block already describing
// version, which is what a maintainer writes by hand before cutting a release.
// The script deliberately does not rewrite these links: it only checks them,
// because prose about a release cannot be generated from its number.
func chartFixture(version string) string {
	return strings.ReplaceAll(chartTemplate, "vLINKED", "v"+strings.TrimPrefix(version, "v"))
}

func TestChartVersion(t *testing.T) {
	tests := []struct {
		name    string
		chart   string
		version string
		wantErr string
		want    []string
	}{
		{
			name:    "sets all three versions",
			chart:   chartFixture("0.2.3"),
			version: "v0.2.3",
			want: []string{
				"version: 0.2.3",
				`appVersion: "0.2.3"`,
				"image: ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:0.2.3",
			},
		},
		{
			name:    "a v prefix is optional",
			chart:   chartFixture("1.0.0"),
			version: "1.0.0",
			want:    []string{"version: 1.0.0", `appVersion: "1.0.0"`},
		},
		{
			name:    "a chart missing the images annotation is an error",
			chart:   strings.ReplaceAll(chartFixture("0.2.0"), "  artifacthub.io/images: |\n    - name: oom-oracle\n      image: ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:0.1.0\n", ""),
			version: "v0.2.0",
			wantErr: "did not set",
		},
		{
			name:    "a non-version is rejected",
			chart:   chartFixture("0.1.0"),
			version: "latest",
			wantErr: "not a version",
		},
		{
			// Artifact Hub would publish the new version's listing with the
			// previous version's release notes, and every other check in the
			// pipeline would pass: the chart version, appVersion and image tag
			// are all correct. Only the prose is a release behind.
			name:    "a changes block still linking the previous release is an error",
			chart:   chartFixture("0.1.0"),
			version: "v0.2.0",
			wantErr: "points at a previous release",
		},
		{
			name:    "a chart with no changes annotation is an error",
			chart:   "apiVersion: v2\nversion: 0.1.0\nappVersion: \"0.1.0\"\n",
			version: "v0.2.0",
			wantErr: "no artifacthub.io/changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			chart := write(t, dir, "Chart.yaml", tt.chart)

			_, stderr, err := run(t, "chart-version.sh", tt.version, chart)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected failure, got success")
				}
				if !strings.Contains(stderr, tt.wantErr) {
					t.Errorf("stderr = %q, want it to mention %q", stderr, tt.wantErr)
				}
				if got := read(t, chart); got != tt.chart {
					t.Errorf("chart was modified by a failing run")
				}
				return
			}
			if err != nil {
				t.Fatalf("chart-version.sh: %v\nstderr: %s", err, stderr)
			}

			got := read(t, chart)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q\n%s", want, got)
				}
			}
			// The old version must be gone entirely, not merely outnumbered.
			if strings.Contains(got, "0.1.0") && tt.version != "v0.1.0" {
				t.Errorf("the previous version survived somewhere\n%s", got)
			}
		})
	}
}

// TestChartVersionAgainstTheRealChart guards the coupling between this script
// and the file it edits: a reshuffled Chart.yaml that the awk no longer matches
// would fail the release after the image had already been pushed.
func TestChartVersionAgainstTheRealChart(t *testing.T) {
	dir := t.TempDir()
	committed := read(t, "../charts/oom-oracle/Chart.yaml")

	// The changes block is hand-written per release, so stamp it to the target
	// here. Leaving it stale would test the staleness guard, which has its own
	// case above, instead of the awk rewriting this test exists for.
	stamped := releaseTag.ReplaceAllString(committed, "releases/tag/v9.8.7")
	chart := write(t, dir, "Chart.yaml", stamped)

	mustRun(t, "chart-version.sh", "v9.8.7", chart)

	got := read(t, chart)
	for _, want := range []string{
		"version: 9.8.7",
		`appVersion: "9.8.7"`,
		"k8s-pod-oom-oracle:9.8.7",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q after rewriting the real chart\n%s", want, got)
		}
	}
}

// TestRealChartCarriesChangesForItsVersion is the check the release runs, made
// unskippable: the committed chart must describe the version it is pinned to,
// so `just release` never reaches the tag before someone notices.
func TestRealChartCarriesChangesForItsVersion(t *testing.T) {
	chart := read(t, "../charts/oom-oracle/Chart.yaml")

	version := chartVersion.FindStringSubmatch(chart)
	if version == nil {
		t.Fatal("the real Chart.yaml has no version")
	}

	tags := releaseTag.FindAllString(chart, -1)
	if len(tags) == 0 {
		t.Fatal("the real Chart.yaml has no artifacthub.io/changes release links")
	}
	want := "releases/tag/v" + version[1]
	for _, tag := range tags {
		if tag != want {
			t.Errorf("changes link %q, want %q: Artifact Hub would show the wrong release's notes", tag, want)
		}
	}
}
