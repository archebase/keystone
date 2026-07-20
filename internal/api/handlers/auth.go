// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
}

// NewAuthHandler constructs an AuthHandler with required dependencies.
func NewAuthHandler(db *sqlx.DB, cfg *config.AuthConfig, hilbertCfg *config.HilbertConfig) *AuthHandler {
	return &AuthHandler{db: db, cfg: cfg, hilbertClient: auth.NewHilbertClient(hilbertCfg)}
}

// LoginRequest is the unified login request body.
// Accepts either "account" (admin or any identity) or the legacy "operator_id"
// field for data collectors. "account" takes priority when both are present.
type LoginRequest struct {
	Account    string `json:"account"`                     // preferred unified field
	OperatorID string `json:"operator_id"`                 // legacy collector field
	DeviceID   string `json:"device_id"`                   // fixed device entry identity
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
	errLoginDeviceRequired            = errors.New("device_id is required")
	errLoginWorkstationNotAssigned    = errors.New("workstation is not assigned for collector and device")
	errLoginDeviceAssignedToCollector = errors.New("device is currently assigned to another collector")
)

type authWorkstationRow struct {
	ID          int64  `db:"id"`
	WorkspaceID int64  `db:"workspace_id"`
	RobotID     int64  `db:"robot_id"`
	Status      string `db:"status"`
}

type authWorkstationInfo struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	RobotID     string `json:"robot_id"`
	Status      string `json:"status"`
}

// AuthMeResponse describes the authenticated global identity and all current workstations.
type AuthMeResponse struct {
	CollectorID    *int64                `json:"collector_id"`
	OperatorID     string                `json:"operator_id"`
	Name           string                `json:"name"`
	Role           string                `json:"role"`
	WorkspaceCount int                   `json:"workspace_count"`
	Workstations   []authWorkstationInfo `json:"workstations"`
}

func (row authWorkstationRow) info() authWorkstationInfo {
	return authWorkstationInfo{
		ID:          strconv.FormatInt(row.ID, 10),
		WorkspaceID: strconv.FormatInt(row.WorkspaceID, 10),
		RobotID:     strconv.FormatInt(row.RobotID, 10),
		Status:      row.Status,
	}
}

// RegisterRoutes registers auth endpoints under the provided router group.
// Routes that require authentication are registered with the appropriate middleware
// applied by the caller (server.go); the route paths are kept here for clarity.
func (h *AuthHandler) RegisterRoutes(r *gin.RouterGroup) {
	// Public — no auth required
	r.POST("/auth/login", h.Login)
	r.POST("/auth/logout", h.Logout)
}

// RegisterAuthenticatedRoutes registers auth endpoints that require a valid JWT.
// jwtAuth and collectorOnly are middleware applied per route group by the caller.
func (h *AuthHandler) RegisterAuthenticatedRoutes(meGroup gin.IRoutes, stationGroup gin.IRoutes) {
	meGroup.GET("", h.Me)
	stationGroup.POST("/break", h.MeStationBreak)
	stationGroup.POST("/end-break", h.MeStationEndBreak)
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

	workstation, err := h.activateLoginWorkstation(
		c.Request.Context(), row.ID, row.OperatorID, strings.TrimSpace(req.DeviceID), time.Now().UTC(),
	)
	if err != nil {
		switch {
		case errors.Is(err, errLoginDeviceRequired):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, errLoginWorkstationNotAssigned):
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		case errors.Is(err, errLoginDeviceAssignedToCollector):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		default:
			logger.Printf("[AUTH] Failed to activate login workstation: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate workstation"})
		}
		return
	}

	// Best-effort: sync the activated workstation status from its active tasks.
	if err := h.syncOneWorkstationStatus(workstation.ID); err != nil {
		logger.Printf("[AUTH] Failed to update workstation status on login (ws=%d): %v", workstation.ID, err)
	}

	claims := auth.NewCollectorWorkstationClaims(
		row.ID, row.OperatorID, workstation.ID, workstation.RobotID, workstation.WorkspaceID,
	)
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

func (h *AuthHandler) activateLoginWorkstation(
	ctx context.Context,
	collectorID int64,
	operatorID string,
	deviceID string,
	now time.Time,
) (authWorkstationRow, error) {
	if deviceID == "" {
		return authWorkstationRow{}, errLoginDeviceRequired
	}
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return authWorkstationRow{}, fmt.Errorf("begin workstation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := " FOR UPDATE"
	if tx.DriverName() == "sqlite" {
		lockClause = ""
	}
	var workstation authWorkstationRow
	if err := tx.GetContext(ctx, &workstation, `
		SELECT ws.id, ws.workspace_id, ws.robot_id, ws.status
		FROM workstations ws
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE ws.data_collector_id = ?
			AND r.device_id = ?
			AND r.status = 'active'
			AND ws.deleted_at IS NULL
		ORDER BY ws.is_current DESC, ws.id DESC
		LIMIT 1`+lockClause, collectorID, deviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return authWorkstationRow{}, errLoginWorkstationNotAssigned
		}
		return authWorkstationRow{}, fmt.Errorf("query login workstation: %w", err)
	}
	allowed, err := services.OperatorHasWorkspaceAccess(ctx, tx, operatorID, workstation.WorkspaceID)
	if err != nil {
		return authWorkstationRow{}, fmt.Errorf("check login workstation access: %w", err)
	}
	if !allowed {
		return authWorkstationRow{}, errLoginWorkstationNotAssigned
	}

	var conflictingCollectorID int64
	err = tx.GetContext(ctx, &conflictingCollectorID, `
		SELECT data_collector_id
		FROM workstations
		WHERE robot_id = ? AND id != ? AND is_current = TRUE AND deleted_at IS NULL
		LIMIT 1`+lockClause, workstation.RobotID, workstation.ID)
	if err == nil && conflictingCollectorID != collectorID {
		return authWorkstationRow{}, errLoginDeviceAssignedToCollector
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return authWorkstationRow{}, fmt.Errorf("query current device binding: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE workstations
		SET is_current = FALSE, status = 'offline', superseded_at = ?, superseded_by = ?, updated_at = ?
		WHERE data_collector_id = ? AND id != ? AND is_current = TRUE AND deleted_at IS NULL
	`, now, workstation.ID, now, collectorID, workstation.ID); err != nil {
		return authWorkstationRow{}, fmt.Errorf("deactivate previous workstation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workstations
		SET is_current = TRUE, status = 'inactive', superseded_at = NULL, superseded_by = NULL, updated_at = ?
		WHERE id = ? AND data_collector_id = ? AND deleted_at IS NULL
	`, now, workstation.ID, collectorID); err != nil {
		return authWorkstationRow{}, fmt.Errorf("activate login workstation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return authWorkstationRow{}, fmt.Errorf("commit workstation activation: %w", err)
	}
	workstation.Status = "inactive"
	return workstation, nil
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
// Requires JWTAuth middleware; works for both admin and data_collector roles.
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
			OperatorID:   "admin",
			Name:         "Administrator",
			Role:         "admin",
			Workstations: []authWorkstationInfo{},
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

	c.JSON(http.StatusOK, AuthMeResponse{
		CollectorID:    &claims.CollectorID,
		OperatorID:     row.OperatorID,
		Name:           row.Name,
		Role:           claims.Role,
		WorkspaceCount: len(workspaceIDs),
		Workstations:   stationInfos,
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
		SELECT id, workspace_id, robot_id, status
		FROM workstations
		WHERE data_collector_id = ? AND workspace_id IN (?) AND is_current = TRUE AND deleted_at IS NULL
		ORDER BY workspace_id, id
	`, collectorID, workspaceIDs)
	if err != nil {
		return nil, err
	}
	rows := []authWorkstationRow{}
	err = h.db.SelectContext(ctx, &rows, h.db.Rebind(query), args...)
	return rows, err
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
