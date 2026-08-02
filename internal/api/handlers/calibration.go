// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services/calibration"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type calibrationManager interface {
	GetSessionStatus(ctx context.Context, sessionID string) (calibration.SessionStatus, error)
	Get(ctx context.Context, captureID string) (calibration.Capture, error)
	List(ctx context.Context, filter calibration.ListFilter) ([]calibration.Capture, int64, error)
	Start(ctx context.Context, captureID, actor string) (calibration.Capture, bool, error)
	CurrentProcessingConfig(ctx context.Context) (calibration.ProcessingConfig, error)
	UpdateProcessingConfig(ctx context.Context, imageRef string, maxConcurrent int, expectedRevisionID int64, actor string) (calibration.ProcessingConfig, error)
	ListProcessingConfigHistory(ctx context.Context, limit, offset int) ([]calibration.ProcessingConfig, error)
}

// CalibrationHandler exposes public Session status and admin Capture controls.
type CalibrationHandler struct {
	manager    calibrationManager
	rateMu     sync.Mutex
	publicRate map[string]publicCalibrationRate
	now        func() time.Time
}

type publicCalibrationRate struct {
	windowStarted time.Time
	requests      int
}

const publicCalibrationRequestsPerMinute = 120

// CalibrationCaptureListResponse is one paginated admin Capture page.
type CalibrationCaptureListResponse struct {
	Items  []calibration.Capture `json:"items"`
	Total  int64                 `json:"total"`
	Limit  int                   `json:"limit"`
	Offset int                   `json:"offset"`
}

// NewCalibrationHandler constructs the calibration HTTP handler.
func NewCalibrationHandler(manager calibrationManager) *CalibrationHandler {
	return &CalibrationHandler{
		manager:    manager,
		publicRate: make(map[string]publicCalibrationRate),
		now:        time.Now,
	}
}

// RegisterPublicRoutes mounts the unauthenticated, non-sensitive status route.
func (h *CalibrationHandler) RegisterPublicRoutes(api *gin.RouterGroup) {
	api.GET("/device/calibration-sessions/:session_id", h.GetSessionStatus)
}

// RegisterAdminRoutes mounts Capture query and Orbit processing routes.
func (h *CalibrationHandler) RegisterAdminRoutes(api *gin.RouterGroup) {
	api.GET("/calibration-captures", h.ListCaptures)
	api.GET("/calibration-captures/:capture_id", h.GetCapture)
	api.POST("/calibration-captures/:capture_id/process", h.ProcessCapture)
	api.GET("/processing-settings/calibration", h.GetProcessingSettings)
	api.PUT("/processing-settings/calibration", h.UpdateProcessingSettings)
	api.GET("/processing-settings/calibration/history", h.ListProcessingSettingsHistory)
}

// UpdateCalibrationProcessingSettingsRequest updates one audited settings revision.
type UpdateCalibrationProcessingSettingsRequest struct {
	ImageRef           string `json:"image_ref" binding:"required"`
	MaxConcurrent      int    `json:"max_concurrent" binding:"required"`
	ExpectedRevisionID int64  `json:"expected_revision_id" binding:"required"`
}

// GetProcessingSettings returns the current calibration Job settings.
// @Summary      Get calibration processing settings
// @Tags         Calibration
// @Produce      json
// @Security     BearerAuth
// @Success      200 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /processing-settings/calibration [get]
func (h *CalibrationHandler) GetProcessingSettings(c *gin.Context) {
	config, err := h.manager.CurrentProcessingConfig(c.Request.Context())
	if err != nil {
		h.writeProcessingSettingsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"config":               config,
		"max_concurrent_limit": calibration.MaxConfigurableConcurrent,
	})
}

// UpdateProcessingSettings appends settings used by future queued Captures.
// @Summary      Update calibration processing settings
// @Tags         Calibration
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request body UpdateCalibrationProcessingSettingsRequest true "Processing settings and current revision"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]interface{}
// @Failure      409 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /processing-settings/calibration [put]
func (h *CalibrationHandler) UpdateProcessingSettings(c *gin.Context) {
	var request UpdateCalibrationProcessingSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedRevisionID <= 0 ||
		request.MaxConcurrent < 1 || request.MaxConcurrent > calibration.MaxConfigurableConcurrent {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "image_ref, max_concurrent between 1 and 100, and positive expected_revision_id are required",
		})
		return
	}
	config, err := h.manager.UpdateProcessingConfig(
		c.Request.Context(),
		request.ImageRef,
		request.MaxConcurrent,
		request.ExpectedRevisionID,
		calibrationActor(c),
	)
	if err != nil {
		h.writeProcessingSettingsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"config":               config,
		"max_concurrent_limit": calibration.MaxConfigurableConcurrent,
	})
}

// ListProcessingSettingsHistory returns append-only calibration settings history.
// @Summary      List calibration processing settings history
// @Tags         Calibration
// @Produce      json
// @Security     BearerAuth
// @Param        limit query int false "Max results"
// @Param        offset query int false "Pagination offset"
// @Success      200 {object} map[string]interface{}
// @Failure      400 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /processing-settings/calibration/history [get]
func (h *CalibrationHandler) ListProcessingSettingsHistory(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	rows, err := h.manager.ListProcessingConfigHistory(c.Request.Context(), pagination.Limit, pagination.Offset)
	if err != nil {
		h.writeProcessingSettingsError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "limit": pagination.Limit, "offset": pagination.Offset})
}

func (h *CalibrationHandler) writeProcessingSettingsError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, calibration.ErrConfigChanged):
		c.JSON(http.StatusConflict, gin.H{
			"error": "calibration processing configuration changed",
			"code":  "config_changed",
		})
	case errors.Is(err, calibration.ErrInvalidMaxConcurrent):
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "calibration max concurrent must be between 1 and 100",
			"code":  "invalid_max_concurrent",
		})
	case strings.Contains(strings.ToLower(err.Error()), "image") ||
		strings.Contains(strings.ToLower(err.Error()), "repository") ||
		strings.Contains(strings.ToLower(err.Error()), "sha256"):
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid calibration image reference",
			"code":  "invalid_image_ref",
		})
	default:
		logger.Printf("[CALIBRATION] processing settings request failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "calibration processing settings operation failed"})
	}
}

// GetSessionStatus returns the non-sensitive status used by an Ego device poller.
// @Summary      Get public calibration Session status
// @Tags         Calibration
// @Produce      json
// @Param        session_id path string true "Calibration Session UUID"
// @Success      200 {object} calibration.SessionStatus
// @Failure      400 {object} map[string]interface{}
// @Failure      404 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /device/calibration-sessions/{session_id} [get]
func (h *CalibrationHandler) GetSessionStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !h.allowPublicSessionStatus(c.ClientIP()) {
		c.Header("Retry-After", "60")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "calibration status rate limit exceeded"})
		return
	}
	sessionID := c.Param("session_id")
	if !isCanonicalV4UUID(sessionID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id must be a canonical UUIDv4"})
		return
	}
	statusValue, err := h.manager.GetSessionStatus(c.Request.Context(), sessionID)
	if err != nil {
		if errors.Is(err, calibration.ErrSessionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "calibration session not found"})
			return
		}
		logger.Printf("[CALIBRATION] get public session status failed session_id=%s: %v", sessionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get calibration session"})
		return
	}
	c.JSON(http.StatusOK, statusValue)
}

func (h *CalibrationHandler) allowPublicSessionStatus(client string) bool {
	now := h.now().UTC()
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	current := h.publicRate[client]
	if current.windowStarted.IsZero() || now.Sub(current.windowStarted) >= time.Minute {
		current = publicCalibrationRate{windowStarted: now}
	}
	current.requests++
	h.publicRate[client] = current
	if len(h.publicRate) > 1024 {
		for key, value := range h.publicRate {
			if now.Sub(value.windowStarted) >= 2*time.Minute {
				delete(h.publicRate, key)
			}
		}
	}
	return current.requests <= publicCalibrationRequestsPerMinute
}

// ListCaptures lists calibration Captures for an administrator.
// @Summary      List calibration Captures
// @Tags         Calibration
// @Produce      json
// @Security     BearerAuth
// @Param        status query string false "Capture status"
// @Param        session_id query string false "Calibration Session UUID"
// @Param        device_id query string false "Device ID"
// @Param        limit query int false "Page size" default(50)
// @Param        offset query int false "Page offset" default(0)
// @Success      200 {object} CalibrationCaptureListResponse
// @Failure      400 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /calibration-captures [get]
func (h *CalibrationHandler) ListCaptures(c *gin.Context) {
	limit, err := boundedQueryInt(c.Query("limit"), 50, 1, 200)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be between 1 and 200"})
		return
	}
	offset, err := boundedQueryInt(c.Query("offset"), 0, 0, 1_000_000)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be between 0 and 1000000"})
		return
	}
	filter := calibration.ListFilter{
		Status:    strings.TrimSpace(c.Query("status")),
		SessionID: strings.TrimSpace(c.Query("session_id")),
		DeviceID:  strings.TrimSpace(c.Query("device_id")),
		Limit:     limit,
		Offset:    offset,
	}
	if filter.Status != "" && !validCalibrationStatus(filter.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid calibration capture status"})
		return
	}
	if filter.SessionID != "" {
		if !isCanonicalV4UUID(filter.SessionID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id must be a canonical UUIDv4"})
			return
		}
	}
	if len(filter.DeviceID) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is too long"})
		return
	}
	captures, total, err := h.manager.List(c.Request.Context(), filter)
	if err != nil {
		logger.Printf("[CALIBRATION] list captures failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list calibration captures"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  captures,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// GetCapture returns one calibration Capture and its TOS result metadata.
// @Summary      Get a calibration Capture
// @Tags         Calibration
// @Produce      json
// @Security     BearerAuth
// @Param        capture_id path string true "Capture UUID"
// @Success      200 {object} calibration.Capture
// @Failure      400 {object} map[string]interface{}
// @Failure      404 {object} map[string]interface{}
// @Failure      500 {object} map[string]interface{}
// @Router       /calibration-captures/{capture_id} [get]
func (h *CalibrationHandler) GetCapture(c *gin.Context) {
	captureID := c.Param("capture_id")
	if !isCanonicalV4UUID(captureID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "capture_id must be a canonical UUIDv4"})
		return
	}
	capture, err := h.manager.Get(c.Request.Context(), captureID)
	if err != nil {
		if errors.Is(err, calibration.ErrCaptureNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "calibration capture not found"})
			return
		}
		logger.Printf("[CALIBRATION] get capture failed capture_id=%s: %v", captureID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get calibration capture"})
		return
	}
	c.JSON(http.StatusOK, capture)
}

// ProcessCapture queues one uploaded Capture for the configured Orbit Job.
// @Summary      Process a calibration Capture with Orbit
// @Tags         Calibration
// @Produce      json
// @Security     BearerAuth
// @Param        capture_id path string true "Capture UUID"
// @Success      202 {object} calibration.Capture
// @Failure      400 {object} map[string]interface{}
// @Failure      404 {object} map[string]interface{}
// @Failure      409 {object} map[string]interface{}
// @Failure      503 {object} map[string]interface{}
// @Router       /calibration-captures/{capture_id}/process [post]
func (h *CalibrationHandler) ProcessCapture(c *gin.Context) {
	captureID := c.Param("capture_id")
	if !isCanonicalV4UUID(captureID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "capture_id must be a canonical UUIDv4"})
		return
	}
	actor := calibrationActor(c)
	capture, _, err := h.manager.Start(c.Request.Context(), captureID, actor)
	if err != nil {
		switch {
		case errors.Is(err, calibration.ErrCaptureNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "calibration capture not found"})
		case errors.Is(err, calibration.ErrDisabled), errors.Is(err, calibration.ErrImageNotConfigured):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "calibration processing is not available"})
		case errors.Is(err, calibration.ErrCaptureUploading),
			errors.Is(err, calibration.ErrCaptureProcessed),
			errors.Is(err, calibration.ErrSessionSucceeded):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			logger.Printf("[CALIBRATION] process capture failed capture_id=%s actor=%s: %v", captureID, actor, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process calibration capture"})
		}
		return
	}
	c.JSON(http.StatusAccepted, capture)
}

func calibrationActor(c *gin.Context) string {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return "admin"
	}
	if actor := strings.TrimSpace(claims.OperatorID); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(claims.Subject); actor != "" {
		return actor
	}
	if actor := strings.TrimSpace(claims.Role); actor != "" {
		return actor
	}
	return "admin"
}

func isCanonicalV4UUID(raw string) bool {
	parsed, err := uuid.Parse(raw)
	return err == nil && parsed.Version() == 4 && parsed.Variant() == uuid.RFC4122 && parsed.String() == raw
}

func boundedQueryInt(raw string, fallback, minimum, maximum int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, errors.New("query integer is out of range")
	}
	return value, nil
}

func validCalibrationStatus(value string) bool {
	switch value {
	case calibration.StatusUploading,
		calibration.StatusUploaded,
		calibration.StatusQueued,
		calibration.StatusSubmitting,
		calibration.StatusPending,
		calibration.StatusRunning,
		calibration.StatusVerifying,
		calibration.StatusSucceeded,
		calibration.StatusFailed,
		calibration.StatusSuperseded:
		return true
	default:
		return false
	}
}
