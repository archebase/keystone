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
	srv := New(&config.Config{
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
	srv := New(&config.Config{
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
