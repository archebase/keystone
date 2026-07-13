// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

var (
	errRegistrationRobotNotFound  = errors.New("robot not found")
	errRegistrationRobotNotActive = errors.New("robot is not active")
)

// DeviceRegistrationHandler handles install-time device registration requests.
type DeviceRegistrationHandler struct {
	db           *sqlx.DB
	callbackURLs callbackURLs
}

// NewDeviceRegistrationHandler creates a new DeviceRegistrationHandler.
func NewDeviceRegistrationHandler(db *sqlx.DB, callbackPublicBaseURL string) *DeviceRegistrationHandler {
	return &DeviceRegistrationHandler{
		db:           db,
		callbackURLs: newCallbackURLs(callbackPublicBaseURL),
	}
}

// DeviceRegistrationRequest represents the request body for device registration.
type DeviceRegistrationRequest struct {
	DeviceID string `json:"device_id"`
}

// DeviceRegistrationResponse represents a successful device registration.
type DeviceRegistrationResponse struct {
	DeviceID          string            `json:"device_id"`
	RobotID           string            `json:"robot_id"`
	WorkspaceID       int64             `json:"workspace_id"`
	WSClientAuthToken string            `json:"ws_client_auth_token"`
	CallbackAllowlist CallbackAllowlist `json:"callback_allowlist"`
}

// RotateWSClientAuthTokenResponse represents a successful token rotation.
type RotateWSClientAuthTokenResponse struct {
	DeviceID          string `json:"device_id"`
	RobotID           string `json:"robot_id"`
	WSClientAuthToken string `json:"ws_client_auth_token"`
	RotatedAt         string `json:"rotated_at"`
}

// RegisterRoutes registers device registration routes.
func (h *DeviceRegistrationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/devices/register", h.RegisterDevice)
}

// RegisterAdminRoutes registers admin-only device credential routes.
func (h *DeviceRegistrationHandler) RegisterAdminRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/robots/:id/ws-client-auth-token/rotate", h.RotateWSClientAuthToken)
}

// RegisterDevice handles install-time robot device registration.
//
// @Summary      Register device
// @Description  Issues a recorder credential for an existing Hilbert device projection
// @Tags         devices
// @Accept       json
// @Produce      json
// @Param        body  body      DeviceRegistrationRequest  true  "Device registration payload"
// @Success      201   {object}  DeviceRegistrationResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /devices/register [post]
func (h *DeviceRegistrationHandler) RegisterDevice(c *gin.Context) {
	var req DeviceRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	deviceID := strings.TrimSpace(req.DeviceID)
	if deviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}

	resp, err := h.registerDevice(deviceID)
	if err != nil {
		switch {
		case errors.Is(err, errRegistrationRobotNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "device not found"})
		case errors.Is(err, errRegistrationRobotNotActive):
			c.JSON(http.StatusBadRequest, gin.H{"error": "device is not active"})
		default:
			logger.Printf("[DEVICE] Failed to register device: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to register device"})
		}
		return
	}

	c.JSON(http.StatusCreated, resp)
}

// RotateWSClientAuthToken handles admin-triggered recorder WebSocket token rotation.
//
// @Summary      Rotate recorder WebSocket client token
// @Description  Revokes active recorder WebSocket client tokens for one robot and returns a new plaintext token once
// @Tags         robots
// @Produce      json
// @Param        id  path      int  true  "Robot ID"
// @Success      200 {object}  RotateWSClientAuthTokenResponse
// @Failure      400 {object}  map[string]string
// @Failure      404 {object}  map[string]string
// @Failure      500 {object}  map[string]string
// @Router       /robots/{id}/ws-client-auth-token/rotate [post]
func (h *DeviceRegistrationHandler) RotateWSClientAuthToken(c *gin.Context) {
	robotID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || robotID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	resp, err := h.rotateWSClientAuthToken(robotID)
	if err != nil {
		switch {
		case errors.Is(err, errRegistrationRobotNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		case errors.Is(err, errRegistrationRobotNotActive):
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot is not active"})
		default:
			logger.Printf("[DEVICE] Failed to rotate ws client auth token: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate ws client auth token"})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func (h *DeviceRegistrationHandler) registerDevice(deviceID string) (DeviceRegistrationResponse, error) {
	tx, err := h.db.Beginx()
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Safe after successful Commit.

	type registrationRobotRow struct {
		ID          int64  `db:"id"`
		DeviceID    string `db:"device_id"`
		WorkspaceID int64  `db:"workspace_id"`
		Status      string `db:"status"`
	}
	query := `
		SELECT id, device_id, workspace_id, status
		FROM robots
		WHERE device_id = ? AND deleted_at IS NULL
		LIMIT 1
	`
	if tx.DriverName() != "sqlite" {
		query += " FOR UPDATE"
	}
	var robot registrationRobotRow
	if err := tx.Get(&robot, query, deviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceRegistrationResponse{}, errRegistrationRobotNotFound
		}
		return DeviceRegistrationResponse{}, fmt.Errorf("query robot: %w", err)
	}
	if robot.Status != "active" {
		return DeviceRegistrationResponse{}, errRegistrationRobotNotActive
	}

	now := time.Now().UTC()
	wsClientAuthToken, err := generateWSClientAuthToken()
	if err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("generate ws client auth token: %w", err)
	}
	if _, err := tx.Exec(`
		UPDATE ws_client_auth_tokens
		SET revoked_at = ?, last_rotated_at = ?
		WHERE robot_id = ? AND revoked_at IS NULL
	`, now, now, robot.ID); err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("revoke active ws client auth tokens: %w", err)
	}
	if err := insertWSClientAuthToken(tx, robot.ID, wsClientAuthToken, now); err != nil {
		return DeviceRegistrationResponse{}, err
	}

	if err := tx.Commit(); err != nil {
		return DeviceRegistrationResponse{}, fmt.Errorf("commit transaction: %w", err)
	}

	return DeviceRegistrationResponse{
		DeviceID:          robot.DeviceID,
		RobotID:           strconv.FormatInt(robot.ID, 10),
		WorkspaceID:       robot.WorkspaceID,
		WSClientAuthToken: wsClientAuthToken,
		CallbackAllowlist: h.callbackURLs.allowlist(),
	}, nil
}

func (h *DeviceRegistrationHandler) rotateWSClientAuthToken(robotID int64) (RotateWSClientAuthTokenResponse, error) {
	tx, err := h.db.Beginx()
	if err != nil {
		return RotateWSClientAuthTokenResponse{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Safe after successful Commit.

	type robotTokenRotationRow struct {
		ID       int64  `db:"id"`
		DeviceID string `db:"device_id"`
		Status   string `db:"status"`
	}

	query := `
		SELECT id, device_id, status
		FROM robots
		WHERE id = ? AND deleted_at IS NULL
	`
	if tx.DriverName() != "sqlite" {
		query += " FOR UPDATE"
	}

	var robot robotTokenRotationRow
	if err := tx.Get(&robot, query, robotID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RotateWSClientAuthTokenResponse{}, errRegistrationRobotNotFound
		}
		return RotateWSClientAuthTokenResponse{}, fmt.Errorf("query robot: %w", err)
	}
	if robot.Status != "active" {
		return RotateWSClientAuthTokenResponse{}, errRegistrationRobotNotActive
	}

	rotatedAt := time.Now().UTC()
	token, err := generateWSClientAuthToken()
	if err != nil {
		return RotateWSClientAuthTokenResponse{}, fmt.Errorf("generate ws client auth token: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE ws_client_auth_tokens
		SET revoked_at = ?, last_rotated_at = ?
		WHERE robot_id = ? AND revoked_at IS NULL
	`, rotatedAt, rotatedAt, robot.ID); err != nil {
		return RotateWSClientAuthTokenResponse{}, fmt.Errorf("revoke active ws client auth tokens: %w", err)
	}

	if err := insertWSClientAuthToken(tx, robot.ID, token, rotatedAt); err != nil {
		return RotateWSClientAuthTokenResponse{}, err
	}

	if err := tx.Commit(); err != nil {
		return RotateWSClientAuthTokenResponse{}, fmt.Errorf("commit transaction: %w", err)
	}

	return RotateWSClientAuthTokenResponse{
		DeviceID:          robot.DeviceID,
		RobotID:           strconv.FormatInt(robot.ID, 10),
		WSClientAuthToken: token,
		RotatedAt:         rotatedAt.Format(time.RFC3339),
	}, nil
}
