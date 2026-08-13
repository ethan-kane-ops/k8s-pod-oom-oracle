package sampler

import (
	"math"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
)

// minTrendSamples is the fewest readings that can define a line.
const minTrendSamples = 2

// Trend describes how a cgroup's memory usage is moving.
//
// This is an ordinary least-squares fit over the buffered samples, nothing
// more. It extrapolates the recent slope and says when that slope would reach
// the limit. Real workloads allocate in steps, plateaus, and sawtooths, so
// TimeToLimit is a rough signal for alerting, not a forecast. RSquared reports
// how well a straight line actually described the window, and callers should
// ignore projections with a poor fit.
type Trend struct {
	// BytesPerSecond is the fitted growth rate. Negative means memory is falling.
	BytesPerSecond float64 `json:"bytesPerSecond"`
	// RSquared is the goodness of fit in [0,1]. A flat line yields 1.
	RSquared float64 `json:"rSquared"`
	// Samples is how many readings informed the fit.
	Samples int `json:"samples"`
	// Window is the time span the fit covers.
	Window time.Duration `json:"window"`
	// TimeToLimit estimates how long until usage reaches the limit. It is only
	// meaningful when Projected is true.
	TimeToLimit time.Duration `json:"timeToLimit"`
	// Projected reports whether TimeToLimit was computed. It is false when
	// memory is flat or falling, the cgroup is uncapped, or the window is too
	// short to fit a line.
	Projected bool `json:"projected"`
}

// Analyse fits a growth rate over the samples and projects time to limit.
//
// Samples must be ordered oldest first, as Ring.Samples returns them.
func Analyse(samples []Sample) Trend {
	trend := Trend{Samples: len(samples)}
	if len(samples) < minTrendSamples {
		return trend
	}

	origin := samples[0].Time
	trend.Window = samples[len(samples)-1].Time.Sub(origin)

	slope, rSquared, ok := fitSlope(samples, origin)
	if !ok {
		return trend
	}
	trend.BytesPerSecond = slope
	trend.RSquared = rSquared

	latest := samples[len(samples)-1]
	trend.TimeToLimit, trend.Projected = projectTimeToLimit(latest.Stats, slope)

	return trend
}

// fitSlope runs an ordinary least-squares regression of bytes against seconds.
// It reports false when every sample shares a timestamp, which leaves the slope
// undefined.
func fitSlope(samples []Sample, origin time.Time) (slope, rSquared float64, ok bool) {
	n := float64(len(samples))

	// Indexed rather than ranged: Sample is a fat value type and these loops run
	// on every trend request.
	var sumX, sumY float64
	for i := range samples {
		sumX += samples[i].Time.Sub(origin).Seconds()
		sumY += float64(samples[i].Stats.Current)
	}
	meanX, meanY := sumX/n, sumY/n

	var covariance, varianceX float64
	for i := range samples {
		dx := samples[i].Time.Sub(origin).Seconds() - meanX
		covariance += dx * (float64(samples[i].Stats.Current) - meanY)
		varianceX += dx * dx
	}
	if varianceX == 0 {
		return 0, 0, false
	}
	slope = covariance / varianceX

	// R^2 compares the residual sum of squares against the total. A perfectly
	// flat series has zero total variance and is, trivially, a perfect fit.
	intercept := meanY - slope*meanX
	var residualSS, totalSS float64
	for i := range samples {
		x := samples[i].Time.Sub(origin).Seconds()
		y := float64(samples[i].Stats.Current)
		predicted := intercept + slope*x
		residualSS += (y - predicted) * (y - predicted)
		totalSS += (y - meanY) * (y - meanY)
	}
	if totalSS == 0 {
		return slope, 1, true
	}

	rSquared = 1 - residualSS/totalSS
	return slope, math.Max(0, math.Min(1, rSquared)), true
}

// projectTimeToLimit extrapolates the fitted slope to the memory ceiling.
func projectTimeToLimit(stats cgroup.MemoryStats, bytesPerSecond float64) (time.Duration, bool) {
	if bytesPerSecond <= 0 || stats.Limit == cgroup.Unlimited || stats.Limit == 0 {
		return 0, false
	}

	headroom := stats.Headroom()
	if headroom == 0 {
		return 0, true
	}

	seconds := float64(headroom) / bytesPerSecond
	// Guard the conversion: a near-zero slope produces a duration that
	// overflows int64 nanoseconds and would wrap to a negative value.
	if seconds >= math.MaxInt64/float64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
}
