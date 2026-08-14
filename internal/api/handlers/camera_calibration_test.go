// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/storage/tos"
)

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
	handler := NewCameraCalibrationHandler(nil, &tos.Reader{}, "test-bucket")
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
