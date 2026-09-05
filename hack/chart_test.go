package hack

import (
	"strings"
	"testing"
)

// The three places Chart.yaml states a version, in the shape the real file uses.
const chartFixture = `apiVersion: v2
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
`

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
			chart:   chartFixture,
			version: "v0.2.3",
			want: []string{
				"version: 0.2.3",
				`appVersion: "0.2.3"`,
				"image: ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle:0.2.3",
			},
		},
		{
			name:    "a v prefix is optional",
			chart:   chartFixture,
			version: "1.0.0",
			want:    []string{"version: 1.0.0", `appVersion: "1.0.0"`},
		},
		{
			name:    "a chart missing the images annotation is an error",
			chart:   "apiVersion: v2\nversion: 0.1.0\nappVersion: \"0.1.0\"\n",
			version: "v0.2.0",
			wantErr: "did not set",
		},
		{
			name:    "a non-version is rejected",
			chart:   chartFixture,
			version: "latest",
			wantErr: "not a version",
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
	chart := write(t, dir, "Chart.yaml", read(t, "../charts/oom-oracle/Chart.yaml"))

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
