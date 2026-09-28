// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"testing"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/services/e6conversion"
)

type stubE6ConversionManager struct{}

func (stubE6ConversionManager) Start(context.Context, int64, string) (e6conversion.Derivative, bool, error) {
	return e6conversion.Derivative{}, false, nil
}

func (stubE6ConversionManager) Get(context.Context, int64) (e6conversion.Derivative, error) {
	return e6conversion.Derivative{}, nil
}

func (stubE6ConversionManager) Retry(context.Context, int64, string) (e6conversion.Derivative, error) {
	return e6conversion.Derivative{}, nil
}

func (stubE6ConversionManager) Cancel(context.Context, int64, string) (e6conversion.Derivative, error) {
	return e6conversion.Derivative{}, nil
}

func (stubE6ConversionManager) RetryQA(context.Context, int64, string) (e6conversion.Derivative, error) {
	return e6conversion.Derivative{}, nil
}

func (stubE6ConversionManager) Logs(context.Context, int64) (string, error) {
	return "", nil
}

func (stubE6ConversionManager) CurrentImageConfig(context.Context) (e6conversion.ImageConfig, error) {
	return e6conversion.ImageConfig{}, nil
}

func (stubE6ConversionManager) UpdateImageConfig(context.Context, string, int, bool, int64, string) (e6conversion.ImageConfig, error) {
	return e6conversion.ImageConfig{}, nil
}

func (stubE6ConversionManager) ListImageConfigHistory(context.Context, int, int) ([]e6conversion.ImageConfig, error) {
	return nil, nil
}

func TestSetE6ConversionManagerWiresManager(t *testing.T) {
	handler := &DataOpsHandler{}
	handler.SetE6ConversionManager(stubE6ConversionManager{})
	if handler.e6Conversion == nil {
		t.Fatal("SetE6ConversionManager did not wire the manager")
	}
}

func TestRegisterE6ConversionRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	handler := &DataOpsHandler{}
	handler.registerE6ConversionRoutes(engine.Group("/api/v1"))

	registered := make(map[string]bool, len(engine.Routes()))
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	want := []string{
		"GET /api/v1/episodes/:id/derivatives/e6-multimodal-conversion",
		"POST /api/v1/episodes/:id/derivatives/e6-multimodal-conversion/process",
		"POST /api/v1/episodes/:id/derivatives/e6-multimodal-conversion/retry",
		"POST /api/v1/episodes/:id/derivatives/e6-multimodal-conversion/cancel",
		"GET /api/v1/episodes/:id/derivatives/e6-multimodal-conversion/logs",
		"POST /api/v1/episodes/:id/derivatives/e6-multimodal-conversion/qa",
		"GET /api/v1/processing-settings/e6-multimodal-conversion",
		"PUT /api/v1/processing-settings/e6-multimodal-conversion",
		"GET /api/v1/processing-settings/e6-multimodal-conversion/history",
	}
	for _, route := range want {
		if !registered[route] {
			t.Errorf("missing E6 conversion route %s", route)
		}
	}
	if len(engine.Routes()) != len(want) {
		t.Errorf("registered %d routes, want %d", len(engine.Routes()), len(want))
	}
}
