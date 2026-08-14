// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/middleware"
)

type fakeCameraCalibrationStore struct {
	bucket     string
	objectName string
	body       []byte
	err        error
}

func (s *fakeCameraCalibrationStore) PutObject(_ context.Context, bucket, objectName string, body []byte) (string, error) {
	s.bucket = bucket
	s.objectName = objectName
	s.body = append([]byte(nil), body...)
	return "", s.err
}

type fakeCameraCalibrationDatabase struct {
	execArgs  []interface{}
	execErr   error
	selectErr error
	result    sql.Result
	items     []cameraCalibrationResponse
}

func (db *fakeCameraCalibrationDatabase) SelectContext(_ context.Context, dest interface{}, _ string, _ ...interface{}) error {
	if db.selectErr != nil {
		return db.selectErr
	}
	items, ok := dest.(*[]cameraCalibrationResponse)
	if !ok {
		return errors.New("unexpected list destination")
	}
	*items = append(*items, db.items...)
	return nil
}

func (db *fakeCameraCalibrationDatabase) ExecContext(_ context.Context, _ string, args ...interface{}) (sql.Result, error) {
	db.execArgs = append([]interface{}(nil), args...)
	if db.execErr != nil {
		return nil, db.execErr
	}
	if db.result != nil {
		return db.result, nil
	}
	return driver.RowsAffected(1), nil
}

type fakeCameraCalibrationResult struct {
	rows    int64
	rowsErr error
}

func (r fakeCameraCalibrationResult) LastInsertId() (int64, error) { return 0, nil }

func (r fakeCameraCalibrationResult) RowsAffected() (int64, error) { return r.rows, r.rowsErr }

func TestCameraCalibrationUploadRequiresCameraSerialFormField(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, err := writer.CreateFormFile("file", "calibration.json")
	if err != nil {
		t.Fatalf("create calibration form file: %v", err)
	}
	if _, err := file.Write([]byte(`{"camera_serial":"camera-in-file"}`)); err != nil {
		t.Fatalf("write calibration form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	router := gin.New()
	handler := NewCameraCalibrationHandler(nil, &fakeCameraCalibrationStore{}, "test-bucket")
	router.POST("/camera-calibrations", handler.Upload)
	req := httptest.NewRequest(http.MethodPost, "/camera-calibrations", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()

	router.ServeHTTP(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if got, want := response.Body.String(), `{"error":"camera_serial is required"}`; got != want {
		t.Fatalf("response body = %s, want %s", got, want)
	}
}

func TestCameraCalibrationUploadRejectsMismatchedDocumentSerial(t *testing.T) {
	store := &fakeCameraCalibrationStore{}
	handler := NewCameraCalibrationHandler(nil, store, "test-bucket")
	response := uploadCameraCalibration(t, handler, "camera-form", `{"camera_serial":"camera-document"}`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if store.objectName != "" {
		t.Fatalf("stored object = %q, want no write", store.objectName)
	}
}

func TestCameraCalibrationUploadReturnsBadGatewayWhenObjectStoreFails(t *testing.T) {
	store := &fakeCameraCalibrationStore{err: errors.New("TOS unavailable")}
	handler := NewCameraCalibrationHandler(nil, store, "test-bucket")
	response := uploadCameraCalibration(t, handler, "camera-1", `{"camera_serial":"camera-1"}`)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusBadGateway, response.Body.String())
	}
	if store.bucket != "test-bucket" || store.objectName == "" || string(store.body) != `{"camera_serial":"camera-1"}` {
		t.Fatalf("object store call = bucket=%q object=%q body=%q", store.bucket, store.objectName, store.body)
	}
}

func TestCameraCalibrationUploadRegistersCurrentManualCalibration(t *testing.T) {
	store := &fakeCameraCalibrationStore{}
	db := &fakeCameraCalibrationDatabase{}
	handler := NewCameraCalibrationHandler(db, store, "test-bucket")
	document := `{"camera_serial":"camera-1"}`
	response := uploadCameraCalibration(t, handler, "camera-1", document)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if store.bucket != "test-bucket" || store.objectName == "" || string(store.body) != document {
		t.Fatalf("object store call = bucket=%q object=%q body=%q", store.bucket, store.objectName, store.body)
	}
	digest := sha256.Sum256([]byte(document))
	if len(db.execArgs) != 6 || db.execArgs[0] != "camera-1" || db.execArgs[1] != "test-bucket" ||
		db.execArgs[2] != store.objectName || db.execArgs[3] != len(document) ||
		db.execArgs[4] != hex.EncodeToString(digest[:]) || db.execArgs[5] != "" {
		t.Fatalf("database upsert args = %#v", db.execArgs)
	}
}

func TestCameraCalibrationUploadReturnsInternalServerErrorWhenRegistrationFails(t *testing.T) {
	store := &fakeCameraCalibrationStore{}
	db := &fakeCameraCalibrationDatabase{execErr: errors.New("database unavailable")}
	handler := NewCameraCalibrationHandler(db, store, "test-bucket")
	response := uploadCameraCalibration(t, handler, "camera-1", `{"camera_serial":"camera-1"}`)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	if store.objectName == "" {
		t.Fatal("object store was not called before registration failure")
	}
}

func TestCameraCalibrationListReturnsInternalServerErrorWhenQueryFails(t *testing.T) {
	handler := NewCameraCalibrationHandler(&fakeCameraCalibrationDatabase{selectErr: errors.New("database unavailable")}, nil, "")
	router := gin.New()
	router.GET("/camera-calibrations", handler.List)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/camera-calibrations", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
}

func TestCameraCalibrationListReturnsCurrentCalibrations(t *testing.T) {
	handler := NewCameraCalibrationHandler(&fakeCameraCalibrationDatabase{items: []cameraCalibrationResponse{{
		CameraSerial: "camera-1", Bucket: "bucket-1", ObjectKey: "derived/camera-1/calibration.json", Source: "manual",
	}}}, nil, "")
	router := gin.New()
	router.GET("/camera-calibrations", handler.List)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/camera-calibrations", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"camera_serial":"camera-1"`) {
		t.Fatalf("response = status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCameraCalibrationDeleteReturnsInternalServerErrorWhenRowsAffectedFails(t *testing.T) {
	handler := NewCameraCalibrationHandler(&fakeCameraCalibrationDatabase{
		result: fakeCameraCalibrationResult{rowsErr: errors.New("rows affected unavailable")},
	}, nil, "")
	router := gin.New()
	router.DELETE("/camera-calibrations/:camera_serial", handler.Delete)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/camera-calibrations/camera-1", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d; body=%s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
}

func TestCameraCalibrationDeleteReturnsExpectedStatuses(t *testing.T) {
	tests := []struct {
		name       string
		db         *fakeCameraCalibrationDatabase
		wantStatus int
	}{
		{name: "deleted", db: &fakeCameraCalibrationDatabase{result: fakeCameraCalibrationResult{rows: 1}}, wantStatus: http.StatusNoContent},
		{name: "not found", db: &fakeCameraCalibrationDatabase{result: fakeCameraCalibrationResult{}}, wantStatus: http.StatusNotFound},
		{name: "execute failure", db: &fakeCameraCalibrationDatabase{execErr: errors.New("database unavailable")}, wantStatus: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewCameraCalibrationHandler(test.db, nil, "")
			router := gin.New()
			router.DELETE("/camera-calibrations/:camera_serial", handler.Delete)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/camera-calibrations/camera-1", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestCameraCalibrationRoutesRequireAdmin(t *testing.T) {
	authConfig := config.AuthConfig{JWTSecret: "camera-calibration-test-secret-at-least-32-bytes", Issuer: "camera-calibration-test", JWTExpiryHours: 1}
	adminToken, err := auth.GenerateToken(auth.NewAdminClaims(), &authConfig)
	if err != nil {
		t.Fatalf("generate admin token: %v", err)
	}
	collectorToken, err := auth.GenerateToken(auth.NewCollectorClaims(7, "collector-7"), &authConfig)
	if err != nil {
		t.Fatalf("generate collector token: %v", err)
	}
	router := gin.New()
	handler := NewCameraCalibrationHandler(&fakeCameraCalibrationDatabase{}, &fakeCameraCalibrationStore{}, "bucket-1")
	handler.RegisterRoutes(router.Group("/api/v1", middleware.JWTAuth(&authConfig), middleware.RequireRole("admin")))

	for _, test := range []struct {
		name, token string
		wantStatus  int
	}{
		{name: "anonymous", wantStatus: http.StatusUnauthorized},
		{name: "collector", token: collectorToken, wantStatus: http.StatusForbidden},
		{name: "admin", token: adminToken, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/camera-calibrations", nil)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d; body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func uploadCameraCalibration(t *testing.T, handler *CameraCalibrationHandler, serial, document string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("camera_serial", serial); err != nil {
		t.Fatalf("write camera serial: %v", err)
	}
	file, err := writer.CreateFormFile("file", "calibration.json")
	if err != nil {
		t.Fatalf("create calibration form file: %v", err)
	}
	if _, err := file.Write([]byte(document)); err != nil {
		t.Fatalf("write calibration form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	router := gin.New()
	router.POST("/camera-calibrations", handler.Upload)
	req := httptest.NewRequest(http.MethodPost, "/camera-calibrations", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}
