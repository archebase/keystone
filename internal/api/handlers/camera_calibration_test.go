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
	"testing"

	"github.com/gin-gonic/gin"
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
	execArgs []interface{}
	execErr  error
}

func (db *fakeCameraCalibrationDatabase) SelectContext(context.Context, interface{}, string, ...interface{}) error {
	return nil
}

func (db *fakeCameraCalibrationDatabase) ExecContext(_ context.Context, _ string, args ...interface{}) (sql.Result, error) {
	db.execArgs = append([]interface{}(nil), args...)
	if db.execErr != nil {
		return nil, db.execErr
	}
	return driver.RowsAffected(1), nil
}

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
