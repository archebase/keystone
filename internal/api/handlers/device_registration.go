// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
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
	db *sqlx.DB
}

// NewDeviceRegistrationHandler creates a new DeviceRegistrationHandler.
func NewDeviceRegistrationHandler(db *sqlx.DB, _ string) *DeviceRegistrationHandler {
	return &DeviceRegistrationHandler{db: db}
}

// RotateDeviceAuthTokenResponse represents a successful token rotation.
type RotateDeviceAuthTokenResponse struct {
	DeviceID        string `json:"device_id"`
	RobotID         string `json:"robot_id"`
	DeviceAuthToken string `json:"device_auth_token"`
	RecoveryEnabled bool   `json:"recovery_enabled"`
	RotatedAt       string `json:"rotated_at"`
}

// RotateDeviceAuthTokenRequest controls whether the newly issued token can recover a lost API key once.
type RotateDeviceAuthTokenRequest struct {
	EnableRecovery bool `json:"enable_recovery"`
}

// RegisterRoutes registers device registration routes.
func (h *DeviceRegistrationHandler) RegisterRoutes(_ *gin.RouterGroup) {
	// Device credentials are administrator-issued only in main-v2.
}

// RegisterAdminRoutes registers admin-only device credential routes.
func (h *DeviceRegistrationHandler) RegisterAdminRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/robots/:id/device-auth-token/rotate", h.RotateDeviceAuthToken)
}

// RotateDeviceAuthToken handles administrator-triggered device token rotation.
//
// @Summary      Rotate device authentication token
// @Description  Revokes active device authentication tokens for one robot and returns a new plaintext token once
// @Tags         robots
// @Produce      json
// @Param        id  path      int  true  "Robot ID"
// @Success      200 {object}  RotateDeviceAuthTokenResponse
// @Failure      400 {object}  map[string]string
// @Failure      404 {object}  map[string]string
// @Failure      500 {object}  map[string]string
// @Router       /robots/{id}/device-auth-token/rotate [post]
func (h *DeviceRegistrationHandler) RotateDeviceAuthToken(c *gin.Context) {
	robotID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || robotID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot id"})
		return
	}

	var req RotateDeviceAuthTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	resp, err := h.rotateDeviceAuthToken(robotID, req.EnableRecovery)
	if err != nil {
		switch {
		case errors.Is(err, errRegistrationRobotNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
		case errors.Is(err, errRegistrationRobotNotActive):
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot is not active"})
		default:
			logger.Printf("[DEVICE] Failed to rotate device auth token: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate ws client auth token"})
		}
		return
	}

	c.JSON(http.StatusOK, resp)
}

func (h *DeviceRegistrationHandler) rotateDeviceAuthToken(robotID int64, enableRecovery bool) (RotateDeviceAuthTokenResponse, error) {
	tx, err := h.db.Beginx()
	if err != nil {
		return RotateDeviceAuthTokenResponse{}, fmt.Errorf("begin transaction: %w", err)
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
			return RotateDeviceAuthTokenResponse{}, errRegistrationRobotNotFound
		}
		return RotateDeviceAuthTokenResponse{}, fmt.Errorf("query robot: %w", err)
	}
	if robot.Status != "active" {
		return RotateDeviceAuthTokenResponse{}, errRegistrationRobotNotActive
	}

	rotatedAt := time.Now().UTC()
	token, err := generateWSClientAuthToken()
	if err != nil {
		return RotateDeviceAuthTokenResponse{}, fmt.Errorf("generate device auth token: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE ws_client_auth_tokens
		SET revoked_at = ?, last_rotated_at = ?
		WHERE robot_id = ? AND revoked_at IS NULL
	`, rotatedAt, rotatedAt, robot.ID); err != nil {
		return RotateDeviceAuthTokenResponse{}, fmt.Errorf("revoke active device auth tokens: %w", err)
	}

	if err := insertWSClientAuthToken(tx, robot.ID, token, rotatedAt, enableRecovery); err != nil {
		return RotateDeviceAuthTokenResponse{}, err
	}

	if err := tx.Commit(); err != nil {
		return RotateDeviceAuthTokenResponse{}, fmt.Errorf("commit transaction: %w", err)
	}

	return RotateDeviceAuthTokenResponse{
		DeviceID:        robot.DeviceID,
		RobotID:         strconv.FormatInt(robot.ID, 10),
		DeviceAuthToken: token,
		RecoveryEnabled: enableRecovery,
		RotatedAt:       rotatedAt.Format(time.RFC3339),
	}, nil
}
