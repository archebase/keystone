// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/storage/tos"
)

const maxCameraCalibrationBytes = 4 << 20

type CameraCalibrationHandler struct {
	db      *sqlx.DB
	objects *tos.Reader
	bucket  string
}

func NewCameraCalibrationHandler(db *sqlx.DB, objects *tos.Reader, bucket string) *CameraCalibrationHandler {
	return &CameraCalibrationHandler{db: db, objects: objects, bucket: strings.TrimSpace(bucket)}
}

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

func (h *CameraCalibrationHandler) List(c *gin.Context) {
	items := []cameraCalibrationResponse{}
	if err := h.db.SelectContext(c.Request.Context(), &items, `SELECT camera_serial, bucket, object_key, size_bytes, sha256, source, calibration_session_id, capture_id, updated_by, updated_at FROM camera_calibrations ORDER BY updated_at DESC, camera_serial`); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list camera calibrations"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

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
	serial, _ := document["camera_serial"].(string)
	serial = strings.TrimSpace(serial)
	if serial == "" || len(serial) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "calibration.json camera_serial is required"})
		return
	}
	digest := sha256.Sum256(data)
	objectKey := path.Join("derived/camera-calibrations", serial, uuid.NewString(), "calibration.json")
	if _, err := h.objects.PutObject(c.Request.Context(), h.bucket, objectKey, data); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to store calibration file"})
		return
	}
	actor, _ := c.Get("username")
	if _, err := h.db.ExecContext(c.Request.Context(), `INSERT INTO camera_calibrations (camera_serial, bucket, object_key, size_bytes, sha256, source, updated_by) VALUES (?, ?, ?, ?, ?, 'manual', ?) ON DUPLICATE KEY UPDATE bucket=VALUES(bucket), object_key=VALUES(object_key), size_bytes=VALUES(size_bytes), sha256=VALUES(sha256), source='manual', calibration_session_id=NULL, capture_id=NULL, updated_by=VALUES(updated_by), updated_at=CURRENT_TIMESTAMP`, serial, h.bucket, objectKey, len(data), hex.EncodeToString(digest[:]), fmt.Sprint(actor)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register calibration file"})
		return
	}
	c.Status(http.StatusNoContent)
}

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
