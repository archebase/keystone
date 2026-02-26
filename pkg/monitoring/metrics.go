// monitoring/metrics.go - Monitoring metrics
package monitoring

import (
	"log"
	"sync/atomic"
	"time"
)

// Metrics monitoring metrics
type Metrics struct {
	// Counters
	requestCount atomic.Int64
	errorCount   atomic.Int64

	// Timestamp
	startTime time.Time
}

// NewMetrics creates monitoring metrics
func NewMetrics() *Metrics {
	m := &Metrics{
		startTime: time.Now(),
	}
	log.Println("[MONITORING] Metrics initialized")
	return m
}

// IncRequestCount increments request count
func (m *Metrics) IncRequestCount() {
	m.requestCount.Add(1)
}

// IncErrorCount increments error count
func (m *Metrics) IncErrorCount() {
	m.errorCount.Add(1)
}

// GetRequestCount returns request count
func (m *Metrics) GetRequestCount() int64 {
	return m.requestCount.Load()
}

// GetErrorCount returns error count
func (m *Metrics) GetErrorCount() int64 {
	return m.errorCount.Load()
}

// GetUptime returns uptime duration
func (m *Metrics) GetUptime() time.Duration {
	return time.Since(m.startTime)
}

// HealthStatus health status
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)
