// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package monitoring provides monitoring metrics
package monitoring

import (
	"sync/atomic"
	"time"

	"archebase.com/keystone-edge/internal/logger"
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
	logger.Println("[MONITORING] Metrics initialized")
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

// Health status constants
const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)
