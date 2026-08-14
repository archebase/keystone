// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxCameraCalibrationBytes = 4 << 20

var cameraSerialPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

// CameraCalibrationHandler manages the current calibration file for each camera.
type CameraCalibrationHandler struct {
	db      cameraCalibrationDatabase
	objects cameraCalibrationObjectStore
	bucket  string
}

type cameraCalibrationDatabase interface {
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

type cameraCalibrationObjectStore interface {
	PutObject(ctx context.Context, bucket, objectName string, body []byte) (string, error)
}

// NewCameraCalibrationHandler creates a handler backed by the supplied object store.
func NewCameraCalibrationHandler(db cameraCalibrationDatabase, objects cameraCalibrationObjectStore, bucket string) *CameraCalibrationHandler {
	return &CameraCalibrationHandler{db: db, objects: objects, bucket: strings.TrimSpace(bucket)}
}

// RegisterRoutes registers current camera-calibration management endpoints.
func (h *CameraCalibrationHandler) RegisterRoutes(api *gin.RouterGroup) {
	api.GET("/camera-calibrations", h.List)
	api.POST("/camera-calibrations", h.Upload)
	api.DELETE("/camera-calibrations/:camera_serial", h.Delete)
}

type cameraCalibrationResponse struct {
	CameraSerial string    `db:"camera_serial" json:"camera_serial"`
	Bucket       string    `db:"bucket" json:"bucket"`
	ObjectKey    string    `db:"object_key" json:"object_key"`
	SizeBytes    int64     `db:"size_bytes" json:"size_bytes"`
	SHA256       string    `db:"sha256" json:"sha256"`
	Source       string    `db:"source" json:"source"`
	SessionID    *string   `db:"calibration_session_id" json:"calibration_session_id,omitempty"`
	CaptureID    *string   `db:"capture_id" json:"capture_id,omitempty"`
	UpdatedBy    *string   `db:"updated_by" json:"updated_by,omitempty"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// List returns every current camera calibration.
//
// @Summary      List current camera calibrations
// @Tags         calibration
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]string
// @Router       /camera-calibrations [get]
func (h *CameraCalibrationHandler) List(c *gin.Context) {
	items := []cameraCalibrationResponse{}
	if err := h.db.SelectContext(c.Request.Context(), &items, `SELECT camera_serial, bucket, object_key, size_bytes, sha256, source, calibration_session_id, capture_id, updated_by, updated_at FROM camera_calibrations ORDER BY updated_at DESC, camera_serial`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list camera calibrations"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// Upload stores a manually uploaded calibration.json as the current camera calibration.
//
// @Summary      Upload current camera calibration
// @Tags         calibration
// @Accept       mpfd
// @Produce      json
// @Param        camera_serial  formData  string  true  "Camera serial"
// @Param        file           formData  file    true  "calibration.json"
// @Success      204
// @Failure      400  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /camera-calibrations [post]
func (h *CameraCalibrationHandler) Upload(c *gin.Context) {
	if h.objects == nil || h.bucket == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "calibration storage is not configured"})
		return
	}
	file, err := c.FormFile("file")
	if err != nil || file.Filename != "calibration.json" || file.Size <= 0 || file.Size > maxCameraCalibrationBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a calibration.json file up to 4 MiB is required"})
		return
	}
	stream, err := file.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read calibration file"})
		return
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, maxCameraCalibrationBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxCameraCalibrationBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read calibration file"})
		return
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil || len(document) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "calibration.json must contain a JSON object"})
		return
	}
	serial := strings.TrimSpace(c.PostForm("camera_serial"))
	if !cameraSerialPattern.MatchString(serial) || serial == "." || serial == ".." {
		c.JSON(http.StatusBadRequest, gin.H{"error": "camera_serial is required"})
		return
	}
	if documentSerial, _ := document["camera_serial"].(string); strings.TrimSpace(documentSerial) != "" &&
		strings.TrimSpace(documentSerial) != serial {
		c.JSON(http.StatusBadRequest, gin.H{"error": "camera_serial does not match calibration.json"})
		return
	}
	digest := sha256.Sum256(data)
	objectKey := path.Join("derived/camera-calibrations", serial, uuid.NewString(), "calibration.json")
	if _, err := h.objects.PutObject(c.Request.Context(), h.bucket, objectKey, data); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store calibration file"})
		return
	}
	actor := ""
	if claims := middleware.GetClaims(c); claims != nil {
		actor = strings.TrimSpace(claims.OperatorID)
	}
	if _, err := h.db.ExecContext(c.Request.Context(), `INSERT INTO camera_calibrations (camera_serial, bucket, object_key, size_bytes, sha256, source, updated_by) VALUES (?, ?, ?, ?, ?, 'manual', ?) ON DUPLICATE KEY UPDATE bucket=VALUES(bucket), object_key=VALUES(object_key), size_bytes=VALUES(size_bytes), sha256=VALUES(sha256), source='manual', calibration_session_id=NULL, capture_id=NULL, updated_by=VALUES(updated_by), updated_at=CURRENT_TIMESTAMP`, serial, h.bucket, objectKey, len(data), hex.EncodeToString(digest[:]), fmt.Sprint(actor)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register calibration file"})
		return
	}
	c.Status(http.StatusNoContent)
}

// Delete removes the current calibration record for one camera.
//
// @Summary      Delete current camera calibration
// @Tags         calibration
// @Produce      json
// @Param        camera_serial  path  string  true  "Camera serial"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /camera-calibrations/{camera_serial} [delete]
func (h *CameraCalibrationHandler) Delete(c *gin.Context) {
	serial := strings.TrimSpace(c.Param("camera_serial"))
	result, err := h.db.ExecContext(c.Request.Context(), "DELETE FROM camera_calibrations WHERE camera_serial = ?", serial)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete camera calibration"})
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "camera calibration not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
