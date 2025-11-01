package common

import (
	"sync"
	"time"

	log "github.com/rs/zerolog/log"
)

// Metrics for performance and usage
type Metrics struct {
	mu sync.RWMutex

	// Range GET metrics
	RangeGetBytesTotal   map[string]int64 // digest -> bytes fetched
	RangeGetRequestTotal map[string]int64 // digest -> request count

	// Inflate CPU metrics
	InflateCPUSecondsTotal float64

	// Read path metrics
	ReadHitsTotal   int64
	ReadMissesTotal int64

	// First exec metrics
	FirstExecStartTime time.Time
	FirstExecDuration  time.Duration

	// Layer metrics
	LayerAccessCount map[string]int64 // digest -> access count
}

// NewMetrics creates a new metrics collector
func NewMetrics() *Metrics {
	return &Metrics{
		RangeGetBytesTotal:   make(map[string]int64),
		RangeGetRequestTotal: make(map[string]int64),
		LayerAccessCount:     make(map[string]int64),
	}
}

// RecordRangeGet records a range GET operation
func (m *Metrics) RecordRangeGet(digest string, bytesRead int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.RangeGetBytesTotal[digest] += bytesRead
	m.RangeGetRequestTotal[digest]++

	log.Debug().
		Str("digest", digest).
		Int64("bytes", bytesRead).
		Int64("total_bytes", m.RangeGetBytesTotal[digest]).
		Int64("total_requests", m.RangeGetRequestTotal[digest]).
		Msg("Range GET recorded")
}

// RecordInflateCPU records CPU time spent inflating
func (m *Metrics) RecordInflateCPU(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.InflateCPUSecondsTotal += duration.Seconds()

	log.Debug().
		Float64("duration_seconds", duration.Seconds()).
		Float64("total_seconds", m.InflateCPUSecondsTotal).
		Msg("Inflate CPU recorded")
}

// RecordReadHit records a cache hit
func (m *Metrics) RecordReadHit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ReadHitsTotal++

	if m.ReadHitsTotal%100 == 0 {
		log.Debug().
			Int64("hits", m.ReadHitsTotal).
			Int64("misses", m.ReadMissesTotal).
			Float64("hit_rate", float64(m.ReadHitsTotal)/float64(m.ReadHitsTotal+m.ReadMissesTotal)).
			Msg("Read cache stats")
	}
}

// RecordReadMiss records a cache miss
func (m *Metrics) RecordReadMiss() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ReadMissesTotal++

	if m.ReadMissesTotal%100 == 0 {
		log.Debug().
			Int64("hits", m.ReadHitsTotal).
			Int64("misses", m.ReadMissesTotal).
			Float64("miss_rate", float64(m.ReadMissesTotal)/float64(m.ReadHitsTotal+m.ReadMissesTotal)).
			Msg("Read cache stats")
	}
}

// RecordFirstExec records the start of the first execution
func (m *Metrics) RecordFirstExecStart() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.FirstExecStartTime.IsZero() {
		m.FirstExecStartTime = time.Now()
		log.Info().Msg("First exec started")
	}
}

// RecordFirstExecEnd records the end of the first execution
func (m *Metrics) RecordFirstExecEnd() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.FirstExecStartTime.IsZero() && m.FirstExecDuration == 0 {
		m.FirstExecDuration = time.Since(m.FirstExecStartTime)
		log.Info().
			Float64("duration_ms", float64(m.FirstExecDuration.Milliseconds())).
			Msg("First exec completed")
	}
}

// RecordLayerAccess records access to a specific layer
func (m *Metrics) RecordLayerAccess(digest string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.LayerAccessCount[digest]++

	if m.LayerAccessCount[digest]%50 == 0 {
		log.Debug().
			Str("digest", digest).
			Int64("access_count", m.LayerAccessCount[digest]).
			Msg("Layer access count")
	}
}

// GetStats returns a snapshot of current statistics
func (m *Metrics) GetStats() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Copy maps to avoid concurrent access
	rangeGetBytes := make(map[string]int64)
	rangeGetReqs := make(map[string]int64)
	layerAccess := make(map[string]int64)

	for k, v := range m.RangeGetBytesTotal {
		rangeGetBytes[k] = v
	}
	for k, v := range m.RangeGetRequestTotal {
		rangeGetReqs[k] = v
	}
	for k, v := range m.LayerAccessCount {
		layerAccess[k] = v
	}

	return MetricsSnapshot{
		RangeGetBytesTotal:     rangeGetBytes,
		RangeGetRequestTotal:   rangeGetReqs,
		InflateCPUSecondsTotal: m.InflateCPUSecondsTotal,
		ReadHitsTotal:          m.ReadHitsTotal,
		ReadMissesTotal:        m.ReadMissesTotal,
		FirstExecDuration:      m.FirstExecDuration,
		LayerAccessCount:       layerAccess,
	}
}

// MetricsSnapshot is a point-in-time snapshot of metrics
type MetricsSnapshot struct {
	RangeGetBytesTotal     map[string]int64
	RangeGetRequestTotal   map[string]int64
	InflateCPUSecondsTotal float64
	ReadHitsTotal          int64
	ReadMissesTotal        int64
	FirstExecDuration      time.Duration
	LayerAccessCount       map[string]int64
}

// PrintSummary prints a human-readable summary of metrics
func (s *MetricsSnapshot) PrintSummary() {
	log.Info().Msg("=== Metrics Summary ===")

	// Range GET stats
	totalBytes := int64(0)
	totalRequests := int64(0)
	for _, bytes := range s.RangeGetBytesTotal {
		totalBytes += bytes
	}
	for _, reqs := range s.RangeGetRequestTotal {
		totalRequests += reqs
	}

	log.Info().
		Int64("total_range_get_bytes", totalBytes).
		Int64("total_range_get_requests", totalRequests).
		Msg("Range GET stats")

	// Inflate CPU
	log.Info().
		Float64("inflate_cpu_seconds", s.InflateCPUSecondsTotal).
		Msg("Inflate CPU stats")

	// Read cache stats
	total := s.ReadHitsTotal + s.ReadMissesTotal
	if total > 0 {
		hitRate := float64(s.ReadHitsTotal) / float64(total)
		log.Info().
			Int64("read_hits", s.ReadHitsTotal).
			Int64("read_misses", s.ReadMissesTotal).
			Float64("hit_rate", hitRate).
			Msg("Read cache stats")
	}

	// First exec
	if s.FirstExecDuration > 0 {
		log.Info().
			Float64("first_exec_ms", float64(s.FirstExecDuration.Milliseconds())).
			Msg("First exec latency")
	}

	log.Info().Msg("=== End Metrics Summary ===")
}

// Global metrics instance
var globalMetrics *Metrics
var metricsOnce sync.Once

// GetGlobalMetrics returns the global metrics instance
func GetGlobalMetrics() *Metrics {
	metricsOnce.Do(func() {
		globalMetrics = NewMetrics()
	})
	return globalMetrics
}
