// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// AuthHandler provides authentication-related HTTP handlers.
type AuthHandler struct {
	db            *sqlx.DB
	cfg           *config.AuthConfig
	hilbertClient *auth.HilbertClient
	hilbert       services.HilbertDCPlanBinder
}

// NewAuthHandler constructs an AuthHandler with required dependencies.
func NewAuthHandler(db *sqlx.DB, cfg *config.AuthConfig, hilbertCfg *config.HilbertConfig) *AuthHandler {
	hilbertClient := auth.NewHilbertClient(hilbertCfg)
	return &AuthHandler{db: db, cfg: cfg, hilbertClient: hilbertClient, hilbert: hilbertClient}
}

// LoginRequest is the unified login request body.
// Accepts either "account" (admin or any identity) or the legacy "operator_id"
// field for data collectors. "account" takes priority when both are present.
type LoginRequest struct {
	Account    string `json:"account"`                     // preferred unified field
	OperatorID string `json:"operator_id"`                 // legacy collector field
	Password   string `json:"password" binding:"required"` // #nosec G117 -- request DTO intentionally contains password
}

// LoginResponse is the unified login response.
type LoginResponse struct {
	AccessToken string         `json:"access_token"` // #nosec G117 -- response DTO intentionally returns access token
	TokenType   string         `json:"token_type"`
	ExpiresIn   int            `json:"expires_in"`
	Role        string         `json:"role"`
	Collector   *collectorInfo `json:"collector"`
}

type collectorInfo struct {
	ID         string `json:"id"`
	OperatorID string `json:"operator_id"`
	Name       string `json:"name"`
}

type collectorAuthRow struct {
	ID         int64  `db:"id"`
	Name       string `db:"name"`
	OperatorID string `db:"operator_id"`
	Status     string `db:"status"`
}

var (
	errWorkstationNotAssigned  = errors.New("workstation is not assigned to collector")
	errRecordingActive         = errors.New("workstation has an active recording")
	errInvalidDeviceCredential = errors.New("invalid device credential")
)

type authWorkstationRow struct {
	ID            int64  `db:"id"`
	WorkspaceID   int64  `db:"workspace_id"`
	WorkspaceName string `db:"workspace_name"`
	RobotID       int64  `db:"robot_id"`
	Status        string `db:"status"`
	DeviceID      string `db:"device_id"`
	DeviceName    string `db:"device_name"`
	IsCurrent     bool   `db:"is_current"`
	Occupied      bool   `db:"occupied"`
}

type authWorkstationInfo struct {
	ID            string `json:"id"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	RobotID       string `json:"robot_id"`
	Status        string `json:"status"`
	DeviceID      string `json:"device_id"`
	DeviceName    string `json:"device_name,omitempty"`
	IsCurrent     bool   `json:"is_current"`
	Occupied      bool   `json:"occupied"`
}

// AuthMeResponse describes the authenticated global identity and all current workstations.
type AuthMeResponse struct {
	CollectorID           *int64                `json:"collector_id"`
	OperatorID            string                `json:"operator_id"`
	Name                  string                `json:"name"`
	Role                  string                `json:"role"`
	WorkspaceCount        int                   `json:"workspace_count"`
	Workstations          []authWorkstationInfo `json:"workstations"`
	AvailableWorkstations []authWorkstationInfo `json:"available_workstations"`
}

// WorkstationActivationRequest selects a workstation for Web clients. EgoPortal
// omits workstation_id and proves its registered device in Device-Authorization.
type WorkstationActivationRequest struct {
	WorkstationID int64 `json:"workstation_id"`
}

// WorkstationActivationResponse returns the existing workstation-scoped JWT shape.
type WorkstationActivationResponse struct {
	AccessToken      string              `json:"access_token"`  // #nosec G117 -- response DTO intentionally returns access token
	RefreshToken     string              `json:"refresh_token"` // #nosec G117 -- response DTO intentionally returns refresh token
	RefreshExpiresIn int                 `json:"refresh_expires_in"`
	TokenType        string              `json:"token_type"`
	ExpiresIn        int                 `json:"expires_in"`
	Role             string              `json:"role"`
	Workstation      authWorkstationInfo `json:"workstation"`
}

type WorkstationRefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// WorkstationRefreshResponse returns a renewed workstation-scoped JWT.
type WorkstationRefreshResponse struct {
	AccessToken string              `json:"access_token"` // #nosec G117 -- response DTO intentionally returns access token
	TokenType   string              `json:"token_type"`
	ExpiresIn   int                 `json:"expires_in"`
	Role        string              `json:"role"`
	Workstation authWorkstationInfo `json:"workstation"`
}

func (row authWorkstationRow) info() authWorkstationInfo {
	return authWorkstationInfo{
		ID:            strconv.FormatInt(row.ID, 10),
		WorkspaceID:   strconv.FormatInt(row.WorkspaceID, 10),
		WorkspaceName: row.WorkspaceName,
		RobotID:       strconv.FormatInt(row.RobotID, 10),
		Status:        row.Status,
		DeviceID:      row.DeviceID,
		DeviceName:    row.DeviceName,
		IsCurrent:     row.IsCurrent,
		Occupied:      row.Occupied,
	}
}

// RegisterRoutes registers auth endpoints under the provided router group.
// Routes that require authentication are registered with the appropriate middleware
// applied by the caller (server.go); the route paths are kept here for clarity.
func (h *AuthHandler) RegisterRoutes(r *gin.RouterGroup) {
	// Public — no auth required
	r.POST("/auth/login", h.Login)
	r.POST("/auth/logout", h.Logout)
	r.POST("/auth/workstation/refresh", h.RefreshWorkstation)
}

// RegisterAuthenticatedRoutes registers auth endpoints that require a valid JWT.
// jwtAuth and collectorOnly are middleware applied per route group by the caller.
func (h *AuthHandler) RegisterAuthenticatedRoutes(meGroup gin.IRoutes, stationGroup gin.IRoutes, activationGroup gin.IRoutes) {
	meGroup.GET("", h.Me)
	stationGroup.POST("/break", h.MeStationBreak)
	stationGroup.POST("/end-break", h.MeStationEndBreak)
	activationGroup.POST("/activate", h.ActivateWorkstation)
}

// Login authenticates a user (admin or Hilbert-backed data collector) and returns a JWT.
//
//	@Summary		Unified login
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		LoginRequest	true	"Login credentials"
//	@Success		200		{object}	LoginResponse
//	@Failure		400		{object}	map[string]string
//	@Failure		401		{object}	map[string]string
//	@Router			/auth/login [post]
func (h *AuthHandler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Resolve account: "account" field takes priority over legacy "operator_id".
	account := strings.TrimSpace(req.Account)
	if account == "" {
		account = strings.TrimSpace(req.OperatorID)
	}
	if account == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account or operator_id is required"})
		return
	}

	// 1. Try admin credentials first (env-var based, no DB round-trip).
	adminUser := strings.TrimSpace(h.cfg.AdminUsername)
	adminPass := strings.TrimSpace(h.cfg.AdminPassword)
	if adminUser != "" && adminPass != "" && account == adminUser && req.Password == adminPass {
		claims := auth.NewAdminClaims()
		token, err := auth.GenerateToken(claims, h.cfg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}
		c.JSON(http.StatusOK, LoginResponse{
			AccessToken: token,
			TokenType:   "Bearer",
			ExpiresIn:   h.cfg.JWTExpiryHours * 3600,
			Role:        "admin",
			Collector:   nil,
		})
		return
	}

	// 2. Fall through to Hilbert-backed data collector authentication.
	if h.db == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	if h.hilbertClient == nil || !h.hilbertClient.Configured() {
		logger.Printf("[AUTH] Hilbert authentication is not configured")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication service unavailable"})
		return
	}

	hilbertResult, err := h.hilbertClient.Login(c.Request.Context(), account, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrHilbertInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		logger.Printf("[AUTH] Hilbert authentication failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "authentication service unavailable"})
		return
	}

	hilbertAccount := hilbertResult.Account
	var row collectorAuthRow
	err = h.db.Get(&row, `
		SELECT id, name, operator_id, status
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, hilbertAccount.Code)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusForbidden, gin.H{"error": "collector is not registered in keystone"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	if row.Status != "active" {
		logger.Printf("[AUTH] Keystone collector is inactive: collector=%d operator_id=%s status=%s", row.ID, row.OperatorID, row.Status)
		c.JSON(http.StatusForbidden, gin.H{"error": "collector is inactive"})
		return
	}
	workspaceIDs, err := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, row.OperatorID)
	if err != nil {
		logger.Printf("[AUTH] Failed to resolve collector Workspace access: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if len(workspaceIDs) == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "collector has no accessible workspace"})
		return
	}

	displayName := strings.TrimSpace(hilbertAccount.DisplayName)
	if displayName != "" {
		row.Name = displayName
		_, _ = h.db.Exec("UPDATE data_collectors SET name = ?, last_login_at = ? WHERE id = ?", displayName, time.Now().UTC(), row.ID)
		_, _ = h.db.Exec("UPDATE workstations SET collector_name = ?, updated_at = ? WHERE data_collector_id = ? AND deleted_at IS NULL", displayName, time.Now().UTC(), row.ID)
	} else {
		_, _ = h.db.Exec("UPDATE data_collectors SET last_login_at = ? WHERE id = ?", time.Now().UTC(), row.ID)
	}

	claims := auth.NewCollectorClaims(row.ID, row.OperatorID)
	token, err := auth.GenerateToken(claims, h.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, LoginResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   h.cfg.JWTExpiryHours * 3600,
		Role:        "data_collector",
		Collector: &collectorInfo{
			ID:         strconv.FormatInt(row.ID, 10),
			OperatorID: row.OperatorID,
			Name:       row.Name,
		},
	})
}

// ActivateWorkstation binds an authenticated collector identity to one canonical workstation.
// Web clients submit workstation_id. EgoPortal submits its persistent device credential in
// Device-Authorization and lets Keystone resolve the workstation.
//
//	@Summary		Activate collector workstation
//	@Description	Web selects workstation_id; EgoPortal supplies Device-Authorization instead
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		WorkstationActivationRequest	false	"Web workstation selection"
//	@Success		200		{object}	WorkstationActivationResponse
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Failure		403		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
//	@Router			/auth/workstation/activate [post]
func (h *AuthHandler) ActivateWorkstation(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "collector authentication required"})
		return
	}

	var req WorkstationActivationRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	deviceToken := parseDeviceAuthorization(c.GetHeader("Device-Authorization"))
	if req.WorkstationID <= 0 && deviceToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workstation_id or device credential is required"})
		return
	}
	if req.WorkstationID > 0 && deviceToken != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workstation_id and device credential cannot be combined"})
		return
	}

	workstation, err := h.activateWorkstation(
		c.Request.Context(), claims.CollectorID, claims.OperatorID, req.WorkstationID, deviceToken,
	)
	if err != nil {
		switch {
		case errors.Is(err, errInvalidDeviceCredential):
			c.JSON(http.StatusUnauthorized, gin.H{"code": "invalid_device_credential", "error": err.Error()})
		case errors.Is(err, errWorkstationNotAssigned):
			c.JSON(http.StatusForbidden, gin.H{"code": "workstation_not_assigned", "error": err.Error()})
		case errors.Is(err, errRecordingActive):
			c.JSON(http.StatusConflict, gin.H{"code": "recording_active", "error": err.Error()})
		default:
			logger.Printf("[AUTH] Failed to activate workstation: collector=%d err=%v", claims.CollectorID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate workstation"})
		}
		return
	}

	token, err := auth.GenerateToken(auth.NewCollectorWorkstationClaims(
		claims.CollectorID,
		claims.OperatorID,
		workstation.ID,
		workstation.RobotID,
		workstation.WorkspaceID,
	), h.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	refreshToken, err := auth.GenerateWorkstationRefreshToken(auth.NewCollectorWorkstationRefreshClaims(
		claims.CollectorID,
		claims.OperatorID,
		workstation.ID,
		workstation.RobotID,
		workstation.WorkspaceID,
	), h.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate refresh token"})
		return
	}

	c.JSON(http.StatusOK, WorkstationActivationResponse{
		AccessToken:      token,
		RefreshToken:     refreshToken,
		RefreshExpiresIn: h.cfg.RefreshTokenExpiryHours * 3600,
		TokenType:        "Bearer",
		ExpiresIn:        h.cfg.JWTExpiryHours * 3600,
		Role:             "data_collector",
		Workstation:      workstation.info(),
	})
}

// RefreshWorkstation renews a workstation access token while the workstation remains current.
// It does not activate or take over a workstation.
func (h *AuthHandler) RefreshWorkstation(c *gin.Context) {
	var req WorkstationRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	claims, err := auth.ParseToken(req.RefreshToken, h.cfg)
	if err != nil || claims.TokenType != "workstation_refresh" || claims.Role != "data_collector" || claims.WorkstationID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "invalid_refresh_token", "error": "invalid or expired refresh token"})
		return
	}

	var workstation authWorkstationRow
	err = h.db.GetContext(c.Request.Context(), &workstation, `
		SELECT ws.id, ws.workspace_id, w.name AS workspace_name, ws.robot_id, ws.status,
			r.device_id, COALESCE(r.device_name, '') AS device_name,
			ws.is_current, FALSE AS occupied
		FROM workstations ws
		JOIN data_collectors dc ON dc.id = ws.data_collector_id
		JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		JOIN workspaces w ON w.id = ws.workspace_id AND w.deleted_at IS NULL
		WHERE ws.id = ? AND ws.data_collector_id = ? AND dc.status = 'active'
			AND ws.is_current = TRUE AND ws.deleted_at IS NULL
		LIMIT 1
	`, claims.WorkstationID, claims.CollectorID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"code": "workstation_session_invalid", "error": "workstation session is no longer active"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if workstation.RobotID != claims.RobotID || workstation.WorkspaceID != claims.WorkspaceID {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "invalid_refresh_token", "error": "invalid or expired refresh token"})
		return
	}

	accessToken, err := auth.GenerateToken(auth.NewCollectorWorkstationClaims(
		claims.CollectorID, claims.OperatorID, workstation.ID, workstation.RobotID, workstation.WorkspaceID,
	), h.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}
	c.JSON(http.StatusOK, WorkstationRefreshResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   h.cfg.JWTExpiryHours * 3600,
		Role:        "data_collector",
		Workstation: workstation.info(),
	})
}

func (h *AuthHandler) activateWorkstation(
	ctx context.Context,
	collectorID int64,
	operatorID string,
	requestedWorkstationID int64,
	deviceToken string,
) (authWorkstationRow, error) {
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return authWorkstationRow{}, fmt.Errorf("begin workstation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := " FOR UPDATE"
	if tx.DriverName() == "sqlite" {
		lockClause = ""
	}
	workstation, tokenID, err := resolveActivationWorkstation(
		ctx, tx, collectorID, requestedWorkstationID, deviceToken, lockClause,
	)
	if errors.Is(err, errWorkstationNotAssigned) && deviceToken != "" {
		if err := h.bootstrapUnboundPlanWorkstation(ctx, tx, collectorID, operatorID, deviceToken, lockClause, time.Now().UTC()); err != nil {
			return authWorkstationRow{}, err
		}
		workstation, tokenID, err = resolveActivationWorkstation(
			ctx, tx, collectorID, requestedWorkstationID, deviceToken, lockClause,
		)
	}
	if err != nil {
		return authWorkstationRow{}, err
	}

	allowed, err := services.OperatorHasWorkspaceAccess(ctx, tx, operatorID, workstation.WorkspaceID)
	if err != nil {
		return authWorkstationRow{}, fmt.Errorf("check workspace access: %w", err)
	}
	if err := h.bindUnboundEgoPlans(ctx, tx, operatorID, workstation.WorkspaceID, workstation.DeviceID, time.Now().UTC()); err != nil {
		return authWorkstationRow{}, err
	}
	eligible, err := collectorHasDevicePlan(ctx, tx, operatorID, workstation.WorkspaceID, workstation.DeviceID)
	if err != nil {
		return authWorkstationRow{}, fmt.Errorf("check device plan eligibility: %w", err)
	}
	if !allowed || !eligible {
		return authWorkstationRow{}, errWorkstationNotAssigned
	}

	var current struct {
		ID          int64 `db:"id"`
		CollectorID int64 `db:"data_collector_id"`
	}
	err = tx.GetContext(ctx, &current, `
		SELECT id, data_collector_id
		FROM workstations
		WHERE robot_id = ? AND is_current = TRUE AND deleted_at IS NULL
		LIMIT 1`+lockClause, workstation.RobotID)
	takesOverCurrentSession := err == nil && (current.ID != workstation.ID || current.CollectorID != collectorID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return authWorkstationRow{}, fmt.Errorf("query current workstation: %w", err)
	}

	if !workstation.IsCurrent || takesOverCurrentSession {
		var activeOtherRecording bool
		if err := tx.GetContext(ctx, &activeOtherRecording, `
			SELECT EXISTS(
				SELECT 1
				FROM tasks t
				JOIN workstations task_ws ON task_ws.id = t.workstation_id
				WHERE task_ws.robot_id = ?
					AND t.status = 'in_progress'
					AND t.deleted_at IS NULL
					AND (task_ws.id != ? OR task_ws.data_collector_id != ?)
			)
		`, workstation.RobotID, workstation.ID, collectorID); err != nil {
			return authWorkstationRow{}, fmt.Errorf("query active recording: %w", err)
		}
		if activeOtherRecording {
			return authWorkstationRow{}, errRecordingActive
		}
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE workstations
		SET is_current = FALSE, status = 'offline', superseded_at = ?, superseded_by = ?, updated_at = ?
		WHERE (data_collector_id = ? OR robot_id = ?) AND id != ?
			AND is_current = TRUE AND deleted_at IS NULL
	`, now, workstation.ID, now, collectorID, workstation.RobotID, workstation.ID); err != nil {
		return authWorkstationRow{}, fmt.Errorf("deactivate previous workstation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workstations
		SET is_current = TRUE, superseded_at = NULL, superseded_by = NULL, updated_at = ?
		WHERE id = ? AND data_collector_id = ? AND deleted_at IS NULL
	`, now, workstation.ID, collectorID); err != nil {
		return authWorkstationRow{}, fmt.Errorf("activate workstation: %w", err)
	}
	if tokenID > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE ws_client_auth_tokens SET last_used_at = ? WHERE id = ?`, now, tokenID); err != nil {
			return authWorkstationRow{}, fmt.Errorf("update device credential usage: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return authWorkstationRow{}, fmt.Errorf("commit workstation activation: %w", err)
	}
	if deviceID, parseErr := strconv.ParseInt(strings.TrimSpace(workstation.DeviceID), 10, 64); parseErr == nil && deviceID > 0 {
		if err := services.EnsureUnboundEgoCandidateTasksForWorkstation(ctx, h.db, h.hilbert, workstation.WorkspaceID, operatorID, workstation.ID, deviceID, now); err != nil {
			logger.Printf("[AUTH] Failed to ensure workstation tasks: workstation=%d error=%v", workstation.ID, err)
		}
	}
	if takesOverCurrentSession {
		logger.Printf(
			"[AUTH] Workstation session replaced: robot=%d previous_workstation=%d previous_collector=%d workstation=%d collector=%d",
			workstation.RobotID,
			current.ID,
			current.CollectorID,
			workstation.ID,
			collectorID,
		)
	}

	if err := h.syncOneWorkstationStatus(workstation.ID); err != nil {
		logger.Printf("[AUTH] Failed to sync activated workstation status: workstation=%d err=%v", workstation.ID, err)
	}
	workstation.IsCurrent = true
	workstation.Occupied = false
	if err := h.db.GetContext(ctx, &workstation.Status, `SELECT status FROM workstations WHERE id = ?`, workstation.ID); err != nil {
		workstation.Status = "inactive"
	}
	return workstation, nil
}

func resolveActivationWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	collectorID int64,
	requestedWorkstationID int64,
	deviceToken string,
	lockClause string,
) (authWorkstationRow, int64, error) {
	robotID := int64(0)
	tokenID := int64(0)
	if deviceToken != "" {
		var device struct {
			TokenID int64 `db:"token_id"`
			RobotID int64 `db:"robot_id"`
		}
		if err := tx.GetContext(ctx, &device, `
			SELECT t.id AS token_id, r.id AS robot_id
			FROM ws_client_auth_tokens t
			JOIN robots r ON r.id = t.robot_id
			WHERE t.token_hash = ? AND t.revoked_at IS NULL
				AND r.status = 'active' AND r.deleted_at IS NULL
			LIMIT 1`+lockClause, hashWSClientAuthToken(deviceToken)); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return authWorkstationRow{}, 0, errInvalidDeviceCredential
			}
			return authWorkstationRow{}, 0, fmt.Errorf("query device credential: %w", err)
		}
		robotID = device.RobotID
		tokenID = device.TokenID
	}

	query := `
		SELECT ws.id, ws.workspace_id, w.name AS workspace_name, ws.robot_id, ws.status,
			r.device_id, COALESCE(r.device_name, '') AS device_name,
			ws.is_current, FALSE AS occupied
		FROM workstations ws
		JOIN robots r ON r.id = ws.robot_id
		JOIN workspaces w ON w.id = ws.workspace_id AND w.deleted_at IS NULL
		WHERE ws.data_collector_id = ? AND ws.deleted_at IS NULL
			AND r.status = 'active' AND r.deleted_at IS NULL`
	args := []any{collectorID}
	if robotID > 0 {
		query += " AND ws.robot_id = ?"
		args = append(args, robotID)
	} else {
		query += " AND ws.id = ?"
		args = append(args, requestedWorkstationID)
	}
	query += " ORDER BY ws.is_current DESC, ws.id DESC LIMIT 1" + lockClause

	var workstation authWorkstationRow
	if err := tx.GetContext(ctx, &workstation, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return authWorkstationRow{}, 0, errWorkstationNotAssigned
		}
		return authWorkstationRow{}, 0, fmt.Errorf("query activation workstation: %w", err)
	}
	return workstation, tokenID, nil
}

func (h *AuthHandler) bootstrapUnboundPlanWorkstation(
	ctx context.Context,
	tx *sqlx.Tx,
	collectorID int64,
	operatorID string,
	deviceToken string,
	lockClause string,
	now time.Time,
) error {
	var device struct {
		RobotID     int64  `db:"robot_id"`
		WorkspaceID int64  `db:"workspace_id"`
		DeviceID    string `db:"device_id"`
		DeviceName  string `db:"device_name"`
	}
	if err := tx.GetContext(ctx, &device, `
		SELECT r.id AS robot_id, r.workspace_id, r.device_id,
			COALESCE(r.device_name, '') AS device_name
		FROM ws_client_auth_tokens t
		JOIN robots r ON r.id = t.robot_id
		WHERE t.token_hash = ? AND t.revoked_at IS NULL
			AND r.status = 'active' AND r.deleted_at IS NULL
		LIMIT 1`+lockClause, hashWSClientAuthToken(deviceToken)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errInvalidDeviceCredential
		}
		return fmt.Errorf("query bootstrap device: %w", err)
	}
	allowed, err := services.OperatorHasWorkspaceAccess(ctx, tx, operatorID, device.WorkspaceID)
	if err != nil {
		return fmt.Errorf("check bootstrap workspace access: %w", err)
	}
	if !allowed {
		return errWorkstationNotAssigned
	}
	var planID int64
	if err := tx.GetContext(ctx, &planID, `
		SELECT id FROM dc_plan
		WHERE workspace_id = ? AND operator = ? AND dc_device_id IS NULL
			AND COALESCE(status, 'pending_collection') <> 'collected'
			AND deleted_at IS NULL
		ORDER BY id
		LIMIT 1`+lockClause, device.WorkspaceID, operatorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errWorkstationNotAssigned
		}
		return fmt.Errorf("query bootstrap dc plan: %w", err)
	}
	var collectorName string
	if err := tx.GetContext(ctx, &collectorName, `
		SELECT name FROM data_collectors WHERE id = ? AND deleted_at IS NULL
	`, collectorID); err != nil {
		return fmt.Errorf("query bootstrap collector: %w", err)
	}
	robotName := strings.TrimSpace(device.DeviceName)
	if robotName == "" {
		robotName = device.DeviceID
	}
	metadata := fmt.Sprintf(`{"source":"unbound_dc_plan_device_activation","dc_plan_id":%d,"workspace_id":%d,"operator":%q}`, planID, device.WorkspaceID, operatorID)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workstations (
			robot_id, robot_name, robot_serial, data_collector_id, collector_name,
			collector_operator_id, workspace_id, name, status, metadata,
			created_at, updated_at, is_current
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'offline', ?, ?, ?, FALSE)
	`, device.RobotID, robotName, device.DeviceID, collectorID, collectorName, operatorID,
		device.WorkspaceID, fmt.Sprintf("Hilbert Device %s Workstation", device.DeviceID), metadata, now, now); err != nil {
		return fmt.Errorf("create bootstrap workstation: %w", err)
	}
	return nil
}
func (h *AuthHandler) bindUnboundEgoPlans(
	ctx context.Context,
	tx *sqlx.Tx,
	operatorID string,
	workspaceID int64,
	deviceID string,
	now time.Time,
) error {
	numericDeviceID, err := strconv.ParseInt(strings.TrimSpace(deviceID), 10, 64)
	if err != nil || numericDeviceID <= 0 {
		return errWorkstationNotAssigned
	}
	var deviceName string
	if err := tx.GetContext(ctx, &deviceName, `
		SELECT COALESCE(device_name, device_id) FROM robots
		WHERE workspace_id = ? AND device_id = ? AND deleted_at IS NULL
		LIMIT 1`, workspaceID, deviceID); err != nil {
		return fmt.Errorf("query device name: %w", err)
	}
	planIDs := []int64{}
	if err := tx.SelectContext(ctx, &planIDs, `
		SELECT id FROM dc_plan
		WHERE operator = ? AND workspace_id = ?
			AND dc_device_id IS NULL
			AND COALESCE(status, 'pending_collection') <> 'collected'
			AND deleted_at IS NULL
		ORDER BY id`+projectionLockClause(tx), operatorID, workspaceID); err != nil {
		return fmt.Errorf("query unbound ego plans: %w", err)
	}
	for _, planID := range planIDs {
		if h.hilbert == nil {
			return fmt.Errorf("bind unbound ego plan: Hilbert binder unavailable")
		}
		bound, err := h.hilbert.PatchDCPlanDCDeviceID(ctx, workspaceID, planID, numericDeviceID)
		if err != nil || !bound {
			return fmt.Errorf("patch dc plan %d device: %w", planID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE dc_plan
			SET dc_device_id = ?, dc_device_name = ?, local_updated_at = ?
			WHERE id = ? AND workspace_id = ? AND dc_device_id IS NULL AND deleted_at IS NULL
		`, numericDeviceID, deviceName, now, planID, workspaceID); err != nil {
			return fmt.Errorf("update dc plan %d device projection: %w", planID, err)
		}
	}
	return nil
}
func projectionLockClause(tx *sqlx.Tx) string {
	if tx != nil && tx.DriverName() == "sqlite" {
		return ""
	}
	return " FOR UPDATE"
}

func collectorHasDevicePlan(
	ctx context.Context,
	q sqlx.QueryerContext,
	operatorID string,
	workspaceID int64,
	deviceID string,
) (bool, error) {
	numericDeviceID, err := strconv.ParseInt(strings.TrimSpace(deviceID), 10, 64)
	if err != nil || numericDeviceID <= 0 {
		return false, nil
	}
	var eligible bool
	if err := sqlx.GetContext(ctx, q, &eligible, `
		SELECT EXISTS(
			SELECT 1 FROM dc_plan
			WHERE operator = ? AND workspace_id = ?
				AND (dc_device_id = ? OR dc_device_id IS NULL)
				AND deleted_at IS NULL
		)
	`, operatorID, workspaceID, numericDeviceID); err != nil {
		return false, err
	}
	return eligible, nil
}

func parseDeviceAuthorization(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return ""
}

// Logout acknowledges logout. The client discards the token; if a valid Bearer
// token is present, the handler best-effort sets the workstation status to offline.
func (h *AuthHandler) Logout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != "" {
			if claims, err := auth.ParseToken(parts[1], h.cfg); err == nil && claims.Role == "data_collector" {
				var updateErr error
				now := time.Now().UTC()
				if claims.WorkstationID > 0 {
					_, updateErr = h.db.Exec(`
						UPDATE workstations
						SET status = 'offline', is_current = FALSE, superseded_at = ?, superseded_by = NULL, updated_at = ?
						WHERE id = ? AND data_collector_id = ? AND is_current = TRUE AND deleted_at IS NULL
					`, now, now, claims.WorkstationID, claims.CollectorID)
				} else {
					_, updateErr = h.db.Exec(`
						UPDATE workstations
						SET status = 'offline', updated_at = ?
						WHERE data_collector_id = ? AND is_current = TRUE AND deleted_at IS NULL
					`, now, claims.CollectorID)
				}
				if updateErr != nil {
					logger.Printf("[AUTH] Failed to update workstation statuses on logout (collector=%d): %v", claims.CollectorID, updateErr)
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me returns the current authenticated identity.
// Requires IdentityJWTAuth middleware; works for both admin and data_collector roles.
// Me returns the global collector identity and every current Workspace workstation.
//
// @Summary      Get current identity
// @Description  Returns the authenticated identity; data collectors receive all current workstations across accessible Workspaces
// @Tags         auth
// @Produce      json
// @Success      200 {object} AuthMeResponse
// @Failure      401 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /auth/me [get]
func (h *AuthHandler) Me(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if claims.Role == "admin" {
		c.JSON(http.StatusOK, AuthMeResponse{
			OperatorID:            "admin",
			Name:                  "Administrator",
			Role:                  "admin",
			Workstations:          []authWorkstationInfo{},
			AvailableWorkstations: []authWorkstationInfo{},
		})
		return
	}

	// data_collector path
	var row struct {
		ID         int64  `db:"id"`
		Name       string `db:"name"`
		OperatorID string `db:"operator_id"`
	}
	if err := h.db.Get(&row, `
		SELECT id, name, operator_id
		FROM data_collectors
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, claims.CollectorID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "collector not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	workspaceIDs, err := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, row.OperatorID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	workstations, err := h.currentWorkstations(c.Request.Context(), claims.CollectorID, row.OperatorID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	stationInfos := make([]authWorkstationInfo, 0, len(workstations))
	for _, workstation := range workstations {
		stationInfos = append(stationInfos, workstation.info())
	}
	available, err := h.availableWorkstations(c.Request.Context(), claims.CollectorID, row.OperatorID)
	if err != nil {
		logger.Printf("[AUTH] Failed to resolve available workstations: collector=%d err=%v", claims.CollectorID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	c.JSON(http.StatusOK, AuthMeResponse{
		CollectorID:           &claims.CollectorID,
		OperatorID:            row.OperatorID,
		Name:                  row.Name,
		Role:                  claims.Role,
		WorkspaceCount:        len(workspaceIDs),
		Workstations:          stationInfos,
		AvailableWorkstations: authWorkstationInfos(available),
	})
}

// MeStationBreak sets every non-offline current workstation to break.
func (h *AuthHandler) MeStationBreak(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workstations, err := h.currentWorkstations(c.Request.Context(), claims.CollectorID, claims.OperatorID)
	if err != nil {
		logger.Printf("[AUTH] MeStationBreak: failed to resolve workstations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if len(workstations) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "workstation not assigned"})
		return
	}
	workstationIDs := make([]int64, 0, len(workstations))
	for _, workstation := range workstations {
		workstationIDs = append(workstationIDs, workstation.ID)
	}
	query, args, err := sqlx.In(`
		UPDATE workstations
		SET status = 'break', updated_at = ?
		WHERE id IN (?) AND is_current = TRUE AND status != 'offline' AND deleted_at IS NULL
	`, time.Now().UTC(), workstationIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workstation"})
		return
	}
	result, err := h.db.Exec(h.db.Rebind(query), args...) // #nosec G701 -- query is built by sqlx.In with placeholders and trusted workstation IDs.
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workstation"})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "all workstations are offline"})
		return
	}
	workstations, _ = h.currentWorkstations(c.Request.Context(), claims.CollectorID, claims.OperatorID)
	c.JSON(http.StatusOK, gin.H{"workstations": authWorkstationInfos(workstations)})
}

// MeStationEndBreak restores every non-offline current workstation based on active tasks.
func (h *AuthHandler) MeStationEndBreak(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	workstations, err := h.currentWorkstations(c.Request.Context(), claims.CollectorID, claims.OperatorID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if len(workstations) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "workstation not assigned"})
		return
	}
	updated := 0
	for _, workstation := range workstations {
		if workstation.Status == "offline" {
			continue
		}
		if err := h.syncOneWorkstationStatus(workstation.ID); err != nil {
			logger.Printf("[AUTH] MeStationEndBreak: failed to update workstation %d: %v", workstation.ID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workstation"})
			return
		}
		updated++
	}
	if updated == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "all workstations are offline"})
		return
	}
	workstations, _ = h.currentWorkstations(c.Request.Context(), claims.CollectorID, claims.OperatorID)
	c.JSON(http.StatusOK, gin.H{"workstations": authWorkstationInfos(workstations)})
}

func (h *AuthHandler) currentWorkstations(ctx context.Context, collectorID int64, operatorID string) ([]authWorkstationRow, error) {
	workspaceIDs, err := services.AccessibleWorkspaceIDs(ctx, h.db, operatorID)
	if err != nil || len(workspaceIDs) == 0 {
		return []authWorkstationRow{}, err
	}
	query, args, err := sqlx.In(`
		SELECT ws.id, ws.workspace_id, w.name AS workspace_name, ws.robot_id, ws.status,
			r.device_id, COALESCE(r.device_name, '') AS device_name,
			ws.is_current, FALSE AS occupied
		FROM workstations ws
		JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		JOIN workspaces w ON w.id = ws.workspace_id AND w.deleted_at IS NULL
		WHERE ws.data_collector_id = ? AND ws.workspace_id IN (?)
			AND ws.is_current = TRUE AND ws.deleted_at IS NULL
		ORDER BY ws.workspace_id, ws.id
	`, collectorID, workspaceIDs)
	if err != nil {
		return nil, err
	}
	rows := []authWorkstationRow{}
	err = h.db.SelectContext(ctx, &rows, h.db.Rebind(query), args...)
	return rows, err
}

func (h *AuthHandler) availableWorkstations(
	ctx context.Context,
	collectorID int64,
	operatorID string,
) ([]authWorkstationRow, error) {
	workspaceIDs, err := services.AccessibleWorkspaceIDs(ctx, h.db, operatorID)
	if err != nil || len(workspaceIDs) == 0 {
		return []authWorkstationRow{}, err
	}
	query, args, err := sqlx.In(`
		SELECT ws.id, ws.workspace_id, w.name AS workspace_name, ws.robot_id, ws.status,
			r.device_id, COALESCE(r.device_name, '') AS device_name,
			ws.is_current,
			EXISTS(
				SELECT 1 FROM workstations occupied_ws
				WHERE occupied_ws.robot_id = ws.robot_id
					AND occupied_ws.is_current = TRUE
					AND occupied_ws.deleted_at IS NULL
					AND occupied_ws.data_collector_id != ?
			) AS occupied
		FROM workstations ws
		JOIN robots r ON r.id = ws.robot_id AND r.status = 'active' AND r.deleted_at IS NULL
		JOIN workspaces w ON w.id = ws.workspace_id AND w.deleted_at IS NULL
		WHERE ws.data_collector_id = ? AND ws.workspace_id IN (?)
			AND ws.deleted_at IS NULL
			AND (ws.is_current = TRUE OR ws.superseded_at IS NULL)
		ORDER BY ws.is_current DESC, ws.workspace_id, ws.id
	`, collectorID, collectorID, workspaceIDs)
	if err != nil {
		return nil, err
	}
	rows := []authWorkstationRow{}
	if err := h.db.SelectContext(ctx, &rows, h.db.Rebind(query), args...); err != nil {
		return nil, err
	}
	return rows, nil
}

func (h *AuthHandler) syncOneWorkstationStatus(workstationID int64) error {
	var hasActiveTasks bool
	if err := h.db.Get(&hasActiveTasks, `
		SELECT EXISTS(
			SELECT 1
			FROM tasks
			WHERE workstation_id = ?
				AND status IN ('ready', 'in_progress', 'uploading')
				AND deleted_at IS NULL
		)
	`, workstationID); err != nil {
		return err
	}
	newStatus := "inactive"
	if hasActiveTasks {
		newStatus = "active"
	}
	_, err := h.db.Exec(`
		UPDATE workstations
		SET status = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
	`, newStatus, time.Now().UTC(), workstationID)
	return err
}

func authWorkstationInfos(rows []authWorkstationRow) []authWorkstationInfo {
	infos := make([]authWorkstationInfo, 0, len(rows))
	for _, row := range rows {
		infos = append(infos, row.info())
	}
	return infos
}
