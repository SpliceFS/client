package fetch

import (
	"sync"
	"time"
)

// sampleWeight is the share of a new sample in the moving average. It lets
// the estimate follow a node whose speed changes within a few blocks without
// jumping on every outlier.
const sampleWeight = 0.3

// RateMeter is a Meter keeping an exponentially weighted moving average of
// bytes per second for each source. A failure halves the estimate, so a node
// that keeps failing sinks to the back. It is safe for concurrent use.
type RateMeter struct {
	mu    sync.Mutex
	rates map[string]float64
}

var _ Meter = (*RateMeter)(nil)

// NewMeter returns an empty RateMeter.
func NewMeter() *RateMeter {
	return &RateMeter{rates: make(map[string]float64)}
}

// Observe adds a sample. A fetch without measurable duration says nothing
// about throughput and is ignored.
func (m *RateMeter) Observe(id string, bytes int, elapsed time.Duration) {
	if elapsed <= 0 {
		return
	}
	sample := float64(bytes) / elapsed.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.rates[id]; ok {
		sample = sampleWeight*sample + (1-sampleWeight)*old
	}
	m.rates[id] = sample
}

// Failed halves the estimate of id. A source without an estimate keeps none.
func (m *RateMeter) Failed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.rates[id]; ok {
		m.rates[id] = old / 2
	}
}

// Rate returns the current estimate of id.
func (m *RateMeter) Rate(id string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rate, ok := m.rates[id]
	return rate, ok
}
