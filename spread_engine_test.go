package main

import (
	"math"
	"testing"
)

func TestSpreadStats_RollingWindowTracksCurrentSamples(t *testing.T) {
	stats := NewSpreadStats(3)
	samples := []float64{1, 2, 3, 4, 5}

	for _, sample := range samples {
		stats.Add(sample)
	}

	wantWindow := []float64{3, 4, 5}
	if stats.Count() != len(wantWindow) {
		t.Fatalf("count = %d, want %d", stats.Count(), len(wantWindow))
	}

	wantMean, wantStdDev := stableStats(wantWindow)
	if math.Abs(stats.Mean()-wantMean) > 1e-12 {
		t.Fatalf("mean = %.12f, want %.12f", stats.Mean(), wantMean)
	}
	if math.Abs(stats.StdDev()-wantStdDev) > 1e-12 {
		t.Fatalf("stddev = %.12f, want %.12f", stats.StdDev(), wantStdDev)
	}
}

func TestSpreadStats_StdDevIsStableForLargeOffsets(t *testing.T) {
	stats := NewSpreadStats(8)
	base := 1e12
	window := []float64{
		base + 0,
		base + 1,
		base + 2,
		base + 3,
		base + 4,
		base + 5,
		base + 6,
		base + 7,
	}

	for _, sample := range window {
		stats.Add(sample)
	}

	wantMean, wantStdDev := stableStats(window)
	if math.Abs(stats.Mean()-wantMean) > 1e-6 {
		t.Fatalf("mean = %.6f, want %.6f", stats.Mean(), wantMean)
	}
	if math.Abs(stats.StdDev()-wantStdDev) > 1e-6 {
		t.Fatalf("stddev = %.6f, want %.6f", stats.StdDev(), wantStdDev)
	}
}

func stableStats(samples []float64) (mean float64, stddev float64) {
	if len(samples) == 0 {
		return 0, 0
	}

	for i, sample := range samples {
		delta := sample - mean
		mean += delta / float64(i+1)
		delta2 := sample - mean
		stddev += delta * delta2
	}

	if len(samples) < 2 {
		return mean, 0
	}

	variance := stddev / float64(len(samples))
	if variance < 0 {
		variance = 0
	}
	return mean, math.Sqrt(variance)
}
