// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/services"
)

func TestAxonTransferWriteTimeoutFromConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.TransferConfig
		want time.Duration
	}{
		{name: "nil config", cfg: nil, want: services.DefaultTransferWriteTimeout},
		{name: "zero config", cfg: &config.TransferConfig{}, want: services.DefaultTransferWriteTimeout},
		{name: "custom seconds", cfg: &config.TransferConfig{WriteTimeout: 7}, want: 7 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := axonTransferWriteTimeout(tt.cfg); got != tt.want {
				t.Fatalf("axonTransferWriteTimeout()=%s want=%s", got, tt.want)
			}
		})
	}
}

func TestHTTPHealthRoutes(t *testing.T) {
	srv, err := New(&config.Config{
		Server: config.ServerConfig{
			BindAddr: ":8080",
		},
		AxonTransfer: config.TransferConfig{
			WSPort:    8090,
			MaxEvents: 10,
		},
		AxonRecorder: config.RecorderConfig{
			WSPort:          8091,
			ResponseTimeout: 1,
		},
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []string{
		"/",
		"/api",
		"/api/v1/health",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()

			srv.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d want=%d body=%s", path, w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
}

func TestWebSocketHealthRoutes(t *testing.T) {
	srv, err := New(&config.Config{
		Server: config.ServerConfig{
			BindAddr: ":8080",
		},
		AxonTransfer: config.TransferConfig{
			WSPort:    8090,
			MaxEvents: 10,
		},
		AxonRecorder: config.RecorderConfig{
			WSPort:          8091,
			ResponseTimeout: 1,
		},
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{name: "transfer", handler: srv.transferWSServer.Handler, path: "/transfer"},
		{name: "recorder", handler: srv.recorderWSServer.Handler, path: "/recorder"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			w := httptest.NewRecorder()

			tt.handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("GET %s status=%d want=%d body=%s", tt.path, w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
}

func TestE6ConversionConfigDerivesOutputPrefix(t *testing.T) {
	cfg := e6ConversionConfig(config.DerivativeConfig{
		Enabled:             true,
		OutputBucket:        "bucket",
		OutputPrefix:        "ego/",
		ActiveDeadlineSec:   900,
		TTLSecondsAfterDone: 600,
		PollIntervalSec:     10,
		MaxSourceBytes:      123,
		OrbitLogTailBytes:   4096,
	})
	if !cfg.Enabled || cfg.OutputBucket != "bucket" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.OutputPrefix != "ego/e6-multimodal-conversion" {
		t.Fatalf("OutputPrefix=%q want ego/e6-multimodal-conversion", cfg.OutputPrefix)
	}
	if cfg.ActiveDeadline != 900 || cfg.TTLSecondsAfterDone != 600 ||
		cfg.MaxSourceBytes != 123 || cfg.LogTailBytes != 4096 {
		t.Fatalf("unexpected deadlines/limits: %+v", cfg)
	}
	if cfg.PollInterval != 10*time.Second {
		t.Fatalf("PollInterval=%v want 10s", cfg.PollInterval)
	}
}
