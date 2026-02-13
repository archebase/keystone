// monitoring/metrics_test.go - Monitoring metrics tests
package monitoring

import (
	"sync"
	"testing"
	"time"
)

func TestNewMetrics(t *testing.T) {
	m := NewMetrics()

	if m == nil {
		t.Fatal("NewMetrics() returned nil")
	}

	if m.GetRequestCount() != 0 {
		t.Errorf("NewMetrics().GetRequestCount() = %v, want 0", m.GetRequestCount())
	}

	if m.GetErrorCount() != 0 {
		t.Errorf("NewMetrics().GetErrorCount() = %v, want 0", m.GetErrorCount())
	}

	if m.GetUptime() < 0 {
		t.Errorf("NewMetrics().GetUptime() = %v, should be >= 0", m.GetUptime())
	}
}

func TestMetricsIncRequestCount(t *testing.T) {
	m := NewMetrics()

	// Increment request count
	m.IncRequestCount()
	if got := m.GetRequestCount(); got != 1 {
		t.Errorf("IncRequestCount() then GetRequestCount() = %v, want 1", got)
	}

	// Multiple increments
	for i := 0; i < 5; i++ {
		m.IncRequestCount()
	}
	if got := m.GetRequestCount(); got != 6 {
		t.Errorf("After 5 IncRequestCount() GetRequestCount() = %v, want 6", got)
	}
}

func TestMetricsIncErrorCount(t *testing.T) {
	m := NewMetrics()

	// Increment error count
	m.IncErrorCount()
	if got := m.GetErrorCount(); got != 1 {
		t.Errorf("IncErrorCount() then GetErrorCount() = %v, want 1", got)
	}

	// Multiple increments
	for i := 0; i < 3; i++ {
		m.IncErrorCount()
	}
	if got := m.GetErrorCount(); got != 4 {
		t.Errorf("After 3 IncErrorCount() GetErrorCount() = %v, want 4", got)
	}
}

func TestMetricsConcurrent(t *testing.T) {
	m := NewMetrics()
	const goroutines = 100
	const incrementsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Concurrent increment request count
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				m.IncRequestCount()
			}
		}()
	}

	// Concurrent increment error count
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incrementsPerGoroutine; j++ {
				m.IncErrorCount()
			}
		}()
	}

	wg.Wait()

	wantRequests := int64(goroutines * incrementsPerGoroutine)
	wantErrors := int64(goroutines * incrementsPerGoroutine)

	if got := m.GetRequestCount(); got != wantRequests {
		t.Errorf("Concurrent GetRequestCount() = %v, want %v", got, wantRequests)
	}

	if got := m.GetErrorCount(); got != wantErrors {
		t.Errorf("Concurrent GetErrorCount() = %v, want %v", got, wantErrors)
	}
}

func TestMetricsGetUptime(t *testing.T) {
	m := NewMetrics()

	// Just created, uptime should be short
	uptime1 := m.GetUptime()
	if uptime1 < 0 {
		t.Errorf("GetUptime() = %v, should be >= 0", uptime1)
	}

	// Wait a while
	time.Sleep(10 * time.Millisecond)
	uptime2 := m.GetUptime()

	if uptime2 <= uptime1 {
		t.Errorf("GetUptime() did not increase: %v -> %v", uptime1, uptime2)
	}

	// Uptime should be at least 10ms
	if uptime2 < 10*time.Millisecond {
		t.Errorf("GetUptime() = %v, should be >= 10ms", uptime2)
	}
}

func TestHealthStatus(t *testing.T) {
	tests := []struct {
		name  string
		status HealthStatus
		want  string
	}{
		{"Healthy status", HealthStatusHealthy, "healthy"},
		{"Degraded status", HealthStatusDegraded, "degraded"},
		{"Unhealthy status", HealthStatusUnhealthy, "unhealthy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.status); got != tt.want {
				t.Errorf("HealthStatus = %v, want %v", got, tt.want)
			}
		})
	}
}
