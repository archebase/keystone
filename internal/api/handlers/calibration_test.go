// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services/calibration"
	"archebase.com/keystone-edge/internal/services/deviceauth"
)

func TestCalibrationDeviceSessionStatusUsesAuthenticatedDevice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeCalibrationManager{
		session: calibration.SessionStatus{
			SessionID:           "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			CameraSerial:        "CAMERA-SN-001",
			Status:              calibration.SessionSucceeded,
			SuccessfulCaptureID: "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
			CaptureCount:        4,
			UploadedCount:       4,
			ProcessedCount:      3,
			UpdatedAt:           time.Date(2026, 8, 2, 10, 20, 30, 0, time.UTC),
		},
	}
	handler := NewCalibrationHandler(manager)
	router := gin.New()
	api := router.Group("/api/v1", func(c *gin.Context) {
		c.Set(middleware.DevicePrincipalKey, deviceauth.Principal{RobotID: 101})
	})
	handler.RegisterDeviceRoutes(api)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/device/calibration-sessions/7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		nil,
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != calibration.SessionSucceeded || body["capture_count"] != float64(4) ||
		body["camera_serial"] != "CAMERA-SN-001" {
		t.Fatalf("response = %v", body)
	}
	if manager.sessionRobotID != 101 {
		t.Fatalf("session queried for robot_id = %d, want 101", manager.sessionRobotID)
	}
	for _, sensitive := range []string{"bucket", "object_key", "local_operator", "device_id"} {
		if _, ok := body[sensitive]; ok {
			t.Fatalf("device response exposed %s: %v", sensitive, body)
		}
	}
}

func TestCalibrationDeviceSessionStatusRejectsNonRandomUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCalibrationHandler(&fakeCalibrationManager{})
	router := gin.New()
	api := router.Group("/api/v1", func(c *gin.Context) {
		c.Set(middleware.DevicePrincipalKey, deviceauth.Principal{RobotID: 101})
	})
	handler.RegisterDeviceRoutes(api)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/device/calibration-sessions/f47ac10b-58cc-1372-8567-0e02b2c3d479",
		nil,
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestCalibrationAdminSessionStatusDoesNotUseDeviceIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeCalibrationManager{
		session: calibration.SessionStatus{
			SessionID:    "7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
			Status:       calibration.SessionRunning,
			CaptureCount: 2,
		},
	}
	handler := NewCalibrationHandler(manager)
	router := gin.New()
	api := router.Group("/api/v1")
	handler.RegisterAdminRoutes(api)

	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/calibration-sessions/7f9af590-75c2-47ad-b6e0-76ebf05c44f7",
		nil,
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	if manager.adminSessionID != "7f9af590-75c2-47ad-b6e0-76ebf05c44f7" {
		t.Fatalf("admin session queried for session_id = %q", manager.adminSessionID)
	}
	if manager.sessionRobotID != 0 {
		t.Fatalf("admin session unexpectedly queried for robot_id = %d", manager.sessionRobotID)
	}
}

func TestCalibrationAdminProcessQueuesUploadedCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeCalibrationManager{
		capture: calibration.Capture{
			CaptureSummary: calibration.CaptureSummary{
				CaptureID: "92cd6f2f-d131-4bf0-9b4a-d96258d09011",
				Status:    calibration.StatusQueued,
			},
		},
	}
	handler := NewCalibrationHandler(manager)
	router := gin.New()
	api := router.Group("/api/v1")
	handler.RegisterAdminRoutes(api)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/calibration-captures/92cd6f2f-d131-4bf0-9b4a-d96258d09011/process",
		strings.NewReader(`{}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if manager.startedCaptureID != "92cd6f2f-d131-4bf0-9b4a-d96258d09011" {
		t.Fatalf("started capture = %q", manager.startedCaptureID)
	}
}

func TestCalibrationAdminUpdatesProcessingSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeCalibrationManager{
		processingConfig: calibration.ProcessingConfig{
			ID:            2,
			ImageRef:      "registry.example/archebase/calibration@sha256:" + strings.Repeat("a", 64),
			MaxConcurrent: 3,
			Source:        calibration.ProcessingConfigSourceDatabase,
		},
	}
	handler := NewCalibrationHandler(manager)
	router := gin.New()
	api := router.Group("/api/v1")
	handler.RegisterAdminRoutes(api)

	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/processing-settings/calibration",
		strings.NewReader(`{
			"image_ref":"registry.example/archebase/calibration@sha256:`+strings.Repeat("a", 64)+`",
			"max_concurrent":3,
			"expected_revision_id":1
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if manager.expectedRevisionID != 1 || manager.updatedMaxConcurrent != 3 {
		t.Fatalf("update args revision=%d max=%d", manager.expectedRevisionID, manager.updatedMaxConcurrent)
	}
}

func TestCalibrationAdminProcessingSettingsKeepsDatabaseErrorsInternal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCalibrationHandler(&fakeCalibrationManager{
		updateProcessingConfigErr: errors.New("load image schema: unavailable"),
	})
	router := gin.New()
	api := router.Group("/api/v1")
	handler.RegisterAdminRoutes(api)

	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/processing-settings/calibration",
		strings.NewReader(`{
			"image_ref":"registry.example/archebase/calibration@sha256:`+strings.Repeat("a", 64)+`",
			"max_concurrent":3,
			"expected_revision_id":1
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", response.Code, response.Body.String())
	}
}

func TestCalibrationAdminProcessingSettingsMapsTypedImageError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCalibrationHandler(&fakeCalibrationManager{
		updateProcessingConfigErr: fmt.Errorf("validate request: %w", calibration.ErrInvalidImageRef),
	})
	router := gin.New()
	api := router.Group("/api/v1")
	handler.RegisterAdminRoutes(api)

	request := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/processing-settings/calibration",
		strings.NewReader(`{
			"image_ref":"registry.example/archebase/calibration@sha256:`+strings.Repeat("a", 64)+`",
			"max_concurrent":3,
			"expected_revision_id":1
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["code"] != "invalid_image_ref" {
		t.Fatalf("response code = %v, want invalid_image_ref", body["code"])
	}
}

type fakeCalibrationManager struct {
	session                   calibration.SessionStatus
	capture                   calibration.Capture
	startedCaptureID          string
	processingConfig          calibration.ProcessingConfig
	updateProcessingConfigErr error
	expectedRevisionID        int64
	updatedMaxConcurrent      int
	sessionRobotID            int64
	adminSessionID            string
}

func (f *fakeCalibrationManager) GetSessionStatus(
	_ context.Context,
	_ string,
	robotID int64,
) (calibration.SessionStatus, error) {
	f.sessionRobotID = robotID
	return f.session, nil
}

func (f *fakeCalibrationManager) GetAdminSessionStatus(
	_ context.Context,
	sessionID string,
) (calibration.SessionStatus, error) {
	f.adminSessionID = sessionID
	return f.session, nil
}

func (f *fakeCalibrationManager) Get(context.Context, string) (calibration.Capture, error) {
	return f.capture, nil
}

func (f *fakeCalibrationManager) List(context.Context, calibration.ListFilter) ([]calibration.CaptureSummary, int64, error) {
	return []calibration.CaptureSummary{f.capture.CaptureSummary}, 1, nil
}

func (f *fakeCalibrationManager) Start(_ context.Context, captureID, _ string) (calibration.Capture, bool, error) {
	f.startedCaptureID = captureID
	return f.capture, true, nil
}

func (f *fakeCalibrationManager) CurrentProcessingConfig(context.Context) (calibration.ProcessingConfig, error) {
	return f.processingConfig, nil
}

func (f *fakeCalibrationManager) UpdateProcessingConfig(
	_ context.Context,
	_ string,
	maxConcurrent int,
	expectedRevisionID int64,
	_ string,
) (calibration.ProcessingConfig, error) {
	f.updatedMaxConcurrent = maxConcurrent
	f.expectedRevisionID = expectedRevisionID
	return f.processingConfig, f.updateProcessingConfigErr
}

func (f *fakeCalibrationManager) ListProcessingConfigHistory(context.Context, int, int) ([]calibration.ProcessingConfig, error) {
	return []calibration.ProcessingConfig{f.processingConfig}, nil
}
