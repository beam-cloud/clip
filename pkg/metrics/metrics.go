package metrics

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Metrics collects performance and usage metrics for Clip v2
type Metrics struct {
	mu sync.RWMutex
	
	// Range GET metrics
	RangeGetBytesTotal   map[string]int64 // by digest
	RangeGetCountTotal   map[string]int64 // by digest
	RangeGetDurationNs   map[string]int64 // by digest
	
	// Inflation metrics
	InflateCPUNs         int64
	InflateCountTotal    int64
	
	// Read path metrics
	ReadHitsTotal        int64
	ReadMissesTotal      int64
	ReadBytesTotal       int64
	
	// First exec metrics
	FirstExecStartTime   map[string]time.Time // by container ID
	FirstExecDurationMs  map[string]int64     // by container ID
	
	// Cache metrics
	CacheHitsTotal       int64
	CacheMissesTotal     int64
	CacheSizeBytes       int64
}

// NewMetrics creates a new metrics collector
func NewMetrics() *Metrics {
	return &Metrics{
		RangeGetBytesTotal: make(map[string]int64),
		RangeGetCountTotal: make(map[string]int64),
		RangeGetDurationNs: make(map[string]int64),
		FirstExecStartTime: make(map[string]time.Time),
		FirstExecDurationMs: make(map[string]int64),
	}
}

// RecordRangeGet records a registry range GET operation
func (m *Metrics) RecordRangeGet(digest string, bytes int64, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.RangeGetBytesTotal[digest] += bytes
	m.RangeGetCountTotal[digest]++
	m.RangeGetDurationNs[digest] += duration.Nanoseconds()
	
	log.Debug().
		Str("digest", digest[:12]+"...").
		Int64("bytes", bytes).
		Dur("duration", duration).
		Msg("range GET completed")
}

// RecordInflation records gzip inflation metrics
func (m *Metrics) RecordInflation(cpuTime time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.InflateCPUNs += cpuTime.Nanoseconds()
	m.InflateCountTotal++
	
	log.Debug().
		Dur("cpu_time", cpuTime).
		Int64("total_inflations", m.InflateCountTotal).
		Msg("inflation completed")
}

// RecordRead records FUSE read operation
func (m *Metrics) RecordRead(bytes int64, hit bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.ReadBytesTotal += bytes
	if hit {
		m.ReadHitsTotal++
	} else {
		m.ReadMissesTotal++
	}
	
	log.Debug().
		Int64("bytes", bytes).
		Bool("cache_hit", hit).
		Int64("total_hits", m.ReadHitsTotal).
		Int64("total_misses", m.ReadMissesTotal).
		Msg("read completed")
}

// RecordFirstExecStart records the start of first exec for a container
func (m *Metrics) RecordFirstExecStart(containerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.FirstExecStartTime[containerID] = time.Now()
	
	log.Info().
		Str("container_id", containerID).
		Msg("first exec started")
}

// RecordFirstExecEnd records the completion of first exec for a container
func (m *Metrics) RecordFirstExecEnd(containerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if startTime, exists := m.FirstExecStartTime[containerID]; exists {
		duration := time.Since(startTime)
		m.FirstExecDurationMs[containerID] = duration.Milliseconds()
		delete(m.FirstExecStartTime, containerID)
		
		log.Info().
			Str("container_id", containerID).
			Int64("duration_ms", duration.Milliseconds()).
			Msg("first exec completed")
	}
}

// RecordCacheOperation records cache hit/miss
func (m *Metrics) RecordCacheOperation(hit bool, sizeBytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if hit {
		m.CacheHitsTotal++
	} else {
		m.CacheMissesTotal++
	}
	
	if sizeBytes > 0 {
		m.CacheSizeBytes += sizeBytes
	}
}

// GetPrometheusMetrics returns metrics in Prometheus format
func (m *Metrics) GetPrometheusMetrics() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	metrics := make(map[string]interface{})
	
	// Aggregate range GET metrics
	var totalRangeGetBytes, totalRangeGetCount int64
	for digest, bytes := range m.RangeGetBytesTotal {
		totalRangeGetBytes += bytes
		totalRangeGetCount += m.RangeGetCountTotal[digest]
		
		// Per-digest metrics
		metrics["clip_range_get_bytes_total{digest=\""+digest[:12]+"...\""] = bytes
		metrics["clip_range_get_count_total{digest=\""+digest[:12]+"...\""] = m.RangeGetCountTotal[digest]
	}
	
	metrics["clip_range_get_bytes_total"] = totalRangeGetBytes
	metrics["clip_range_get_count_total"] = totalRangeGetCount
	metrics["clip_inflate_cpu_seconds_total"] = float64(m.InflateCPUNs) / 1e9
	metrics["clip_inflate_count_total"] = m.InflateCountTotal
	metrics["clip_read_hits_total"] = m.ReadHitsTotal
	metrics["clip_read_misses_total"] = m.ReadMissesTotal
	metrics["clip_read_bytes_total"] = m.ReadBytesTotal
	metrics["clip_cache_hits_total"] = m.CacheHitsTotal
	metrics["clip_cache_misses_total"] = m.CacheMissesTotal
	metrics["clip_cache_size_bytes"] = m.CacheSizeBytes
	
	// First exec metrics
	for containerID, durationMs := range m.FirstExecDurationMs {
		metrics["clip_first_exec_ms{container_id=\""+containerID+"\""] = durationMs
	}
	
	return metrics
}

// LogSummary logs a summary of current metrics
func (m *Metrics) LogSummary() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	var totalRangeGetBytes, totalRangeGetCount int64
	for _, bytes := range m.RangeGetBytesTotal {
		totalRangeGetBytes += bytes
	}
	for _, count := range m.RangeGetCountTotal {
		totalRangeGetCount += count
	}
	
	cacheHitRate := float64(0)
	if m.ReadHitsTotal+m.ReadMissesTotal > 0 {
		cacheHitRate = float64(m.ReadHitsTotal) / float64(m.ReadHitsTotal+m.ReadMissesTotal)
	}
	
	log.Info().
		Int64("range_get_bytes", totalRangeGetBytes).
		Int64("range_get_count", totalRangeGetCount).
		Int64("inflate_count", m.InflateCountTotal).
		Float64("inflate_cpu_seconds", float64(m.InflateCPUNs)/1e9).
		Int64("read_hits", m.ReadHitsTotal).
		Int64("read_misses", m.ReadMissesTotal).
		Float64("cache_hit_rate", cacheHitRate).
		Int64("cache_size_bytes", m.CacheSizeBytes).
		Msg("metrics summary")
}

// Global metrics instance
var GlobalMetrics = NewMetrics()

// Convenience functions for global metrics
func RecordRangeGet(digest string, bytes int64, duration time.Duration) {
	GlobalMetrics.RecordRangeGet(digest, bytes, duration)
}

func RecordInflation(cpuTime time.Duration) {
	GlobalMetrics.RecordInflation(cpuTime)
}

func RecordRead(bytes int64, hit bool) {
	GlobalMetrics.RecordRead(bytes, hit)
}

func RecordFirstExecStart(containerID string) {
	GlobalMetrics.RecordFirstExecStart(containerID)
}

func RecordFirstExecEnd(containerID string) {
	GlobalMetrics.RecordFirstExecEnd(containerID)
}

func RecordCacheOperation(hit bool, sizeBytes int64) {
	GlobalMetrics.RecordCacheOperation(hit, sizeBytes)
}

func LogMetricsSummary() {
	GlobalMetrics.LogSummary()
}