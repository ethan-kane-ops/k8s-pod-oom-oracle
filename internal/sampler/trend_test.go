package sampler

import (
	"math"
	"testing"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

// series builds evenly spaced samples, one per second, against a fixed limit.
func series(limit uint64, currents ...uint64) []Sample {
	samples := make([]Sample, len(currents))
	for i, current := range currents {
		samples[i] = Sample{
			Time:  epoch.Add(time.Duration(i) * time.Second),
			Stats: cgroup.MemoryStats{Current: current, Limit: limit},
		}
	}
	return samples
}

func TestAnalyseGrowthRate(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20

	tests := []struct {
		name          string
		samples       []Sample
		wantRate      float64
		wantProjected bool
		wantTTL       time.Duration
	}{
		{
			name:          "steady growth projects to the limit",
			samples:       series(100*mib, 10*mib, 20*mib, 30*mib, 40*mib),
			wantRate:      10 * mib,
			wantProjected: true,
			// 60 MiB of headroom at 10 MiB/s.
			wantTTL: 6 * time.Second,
		},
		{
			name:          "flat memory is not projected",
			samples:       series(100*mib, 50*mib, 50*mib, 50*mib),
			wantRate:      0,
			wantProjected: false,
		},
		{
			name:          "falling memory is not projected",
			samples:       series(100*mib, 40*mib, 30*mib, 20*mib),
			wantRate:      -10 * mib,
			wantProjected: false,
		},
		{
			name:          "uncapped cgroup is never projected",
			samples:       series(cgroup.Unlimited, 10*mib, 20*mib, 30*mib),
			wantRate:      10 * mib,
			wantProjected: false,
		},
		{
			name:          "already at the limit projects zero",
			samples:       series(100*mib, 80*mib, 90*mib, 100*mib),
			wantRate:      10 * mib,
			wantProjected: true,
			wantTTL:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Analyse(tt.samples)

			if math.Abs(got.BytesPerSecond-tt.wantRate) > 1 {
				t.Errorf("BytesPerSecond = %v, want %v", got.BytesPerSecond, tt.wantRate)
			}
			if got.Projected != tt.wantProjected {
				t.Fatalf("Projected = %v, want %v", got.Projected, tt.wantProjected)
			}
			if tt.wantProjected {
				drift := (got.TimeToLimit - tt.wantTTL).Abs()
				if drift > 100*time.Millisecond {
					t.Errorf("TimeToLimit = %v, want %v", got.TimeToLimit, tt.wantTTL)
				}
			}
			if got.Samples != len(tt.samples) {
				t.Errorf("Samples = %d, want %d", got.Samples, len(tt.samples))
			}
		})
	}
}

func TestAnalyseInsufficientSamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		samples []Sample
	}{
		{name: "no samples", samples: nil},
		{name: "single sample cannot define a slope", samples: series(1<<30, 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Analyse(tt.samples)
			if got.Projected {
				t.Error("Projected = true, want false without enough samples")
			}
			if got.BytesPerSecond != 0 {
				t.Errorf("BytesPerSecond = %v, want 0", got.BytesPerSecond)
			}
			if got.Samples != len(tt.samples) {
				t.Errorf("Samples = %d, want %d", got.Samples, len(tt.samples))
			}
		})
	}
}

func TestAnalyseIdenticalTimestamps(t *testing.T) {
	t.Parallel()

	// Every sample at the same instant leaves the slope undefined.
	samples := []Sample{
		{Time: epoch, Stats: cgroup.MemoryStats{Current: 100, Limit: 1000}},
		{Time: epoch, Stats: cgroup.MemoryStats{Current: 200, Limit: 1000}},
		{Time: epoch, Stats: cgroup.MemoryStats{Current: 300, Limit: 1000}},
	}

	got := Analyse(samples)
	if got.Projected {
		t.Error("Projected = true, want false when the time window is zero")
	}
	if got.BytesPerSecond != 0 {
		t.Errorf("BytesPerSecond = %v, want 0", got.BytesPerSecond)
	}
}

func TestAnalyseRSquared(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20

	tests := []struct {
		name    string
		samples []Sample
		wantMin float64
	}{
		{
			name:    "perfectly linear growth fits exactly",
			samples: series(100*mib, 10*mib, 20*mib, 30*mib, 40*mib),
			wantMin: 0.999,
		},
		{
			name:    "flat series is a trivially perfect fit",
			samples: series(100*mib, 50*mib, 50*mib, 50*mib),
			wantMin: 0.999,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Analyse(tt.samples)
			if got.RSquared < tt.wantMin {
				t.Errorf("RSquared = %v, want at least %v", got.RSquared, tt.wantMin)
			}
			if got.RSquared > 1 {
				t.Errorf("RSquared = %v, must never exceed 1", got.RSquared)
			}
		})
	}
}

func TestAnalyseSawtoothHasPoorFit(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20

	// A garbage-collected runtime: allocate, collect, repeat. The mean slope is
	// near zero and a straight line explains almost none of the variance, which
	// is exactly when a projection must not be trusted.
	samples := series(200*mib, 10*mib, 90*mib, 15*mib, 95*mib, 12*mib, 92*mib)

	got := Analyse(samples)
	if got.RSquared > 0.5 {
		t.Errorf("RSquared = %v, want a poor fit for a sawtooth series", got.RSquared)
	}
}

func TestAnalyseWindow(t *testing.T) {
	t.Parallel()

	samples := series(1<<30, 1, 2, 3, 4, 5)

	if got, want := Analyse(samples).Window, 4*time.Second; got != want {
		t.Errorf("Window = %v, want %v", got, want)
	}
}

func TestAnalyseNegligibleSlopeDoesNotOverflow(t *testing.T) {
	t.Parallel()

	// One byte of growth spread over an hour projects further ahead than a
	// time.Duration can hold. The projection must be refused, not wrapped.
	samples := []Sample{
		{Time: epoch, Stats: cgroup.MemoryStats{Current: 1, Limit: math.MaxUint64 - 1}},
		{Time: epoch.Add(time.Hour), Stats: cgroup.MemoryStats{Current: 2, Limit: math.MaxUint64 - 1}},
	}

	got := Analyse(samples)
	if got.Projected {
		t.Errorf("Projected = true with TimeToLimit = %v, want the projection refused", got.TimeToLimit)
	}
	if got.TimeToLimit < 0 {
		t.Errorf("TimeToLimit = %v, must never be negative", got.TimeToLimit)
	}
}
