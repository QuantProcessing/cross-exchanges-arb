package spread

import "math"

// Stats maintains rolling statistics (mean, stddev) over a fixed-size ring buffer.
type Stats struct {
	buf     []float64
	maxSize int
	count   int
	writeAt int
	mean    float64
	stddev  float64
	dirty   bool
}

// NewStats creates a new stats tracker with the given window size.
func NewStats(windowSize int) *Stats {
	if windowSize < 1 {
		windowSize = 1
	}
	return &Stats{
		buf:     make([]float64, windowSize),
		maxSize: windowSize,
		dirty:   true,
	}
}

// Add inserts a new spread value into the ring buffer.
func (s *Stats) Add(spread float64) {
	s.buf[s.writeAt] = spread
	s.writeAt = (s.writeAt + 1) % s.maxSize
	if s.count < s.maxSize {
		s.count++
	}
	s.dirty = true
}

// Count returns the number of samples in the window.
func (s *Stats) Count() int {
	return s.count
}

// Mean returns the rolling mean.
func (s *Stats) Mean() float64 {
	s.recomputeIfDirty()
	return s.mean
}

// StdDev returns the rolling standard deviation.
func (s *Stats) StdDev() float64 {
	s.recomputeIfDirty()
	return s.stddev
}

// ZScore returns the Z-Score of a given spread value relative to the rolling distribution.
func (s *Stats) ZScore(spread float64) float64 {
	sd := s.StdDev()
	if sd < 1e-9 {
		return 0
	}
	return (spread - s.Mean()) / sd
}

func (s *Stats) activeSlice() []float64 {
	if s.count < s.maxSize {
		return s.buf[:s.count]
	}
	result := make([]float64, s.maxSize)
	copy(result, s.buf[s.writeAt:])
	copy(result[s.maxSize-s.writeAt:], s.buf[:s.writeAt])
	return result
}

func (s *Stats) recomputeIfDirty() {
	if !s.dirty {
		return
	}

	data := s.activeSlice()
	if len(data) == 0 {
		s.mean = 0
		s.stddev = 0
		s.dirty = false
		return
	}

	mean := 0.0
	m2 := 0.0
	for i, sample := range data {
		delta := sample - mean
		mean += delta / float64(i+1)
		delta2 := sample - mean
		m2 += delta * delta2
	}

	s.mean = mean
	if len(data) < 2 {
		s.stddev = 0
		s.dirty = false
		return
	}

	variance := m2 / float64(len(data)-1)
	if variance < 0 {
		variance = 0
	}
	s.stddev = math.Sqrt(variance)
	s.dirty = false
}
