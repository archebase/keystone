// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

// AuthHandler provides authentication-related HTTP handlers.
type AuthHandler struct {
	db  *sqlx.DB
	cfg *config.AuthConfig
}

// NewAuthHandler constructs an AuthHandler with required dependencies.
func NewAuthHandler(db *sqlx.DB, cfg *config.AuthConfig) *AuthHandler {
	return &AuthHandler{db: db, cfg: cfg}
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
	ID           int64          `db:"id"`
	Name         string         `db:"name"`
	OperatorID   string         `db:"operator_id"`
	PasswordHash sql.NullString `db:"password_hash"`
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

// Login authenticates a user (admin or data collector) and returns a JWT.
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

	// 2. Fall through to data collector DB lookup.
	if h.db == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	var row collectorAuthRow
	err := h.db.Get(&row, `
		SELECT id, name, operator_id, password_hash
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, account)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	if !row.PasswordHash.Valid || strings.TrimSpace(row.PasswordHash.String) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(row.PasswordHash.String), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	_, _ = h.db.Exec("UPDATE data_collectors SET last_login_at = NOW() WHERE id = ?", row.ID)

	// Best-effort: sync workstation status on login.
	h.syncWorkstationStatusOnLogin(row.ID)

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

// Logout acknowledges logout. The client discards the token; if a valid Bearer
// token is present, the handler best-effort sets the workstation status to offline.
func (h *AuthHandler) Logout(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != "" {
			if claims, err := auth.ParseToken(parts[1], h.cfg); err == nil && claims.Role == "data_collector" {
				var wsID int64
				if err := h.db.Get(&wsID, `
					SELECT id
					FROM workstations
					WHERE data_collector_id = ? AND deleted_at IS NULL
					LIMIT 1
				`, claims.CollectorID); err == nil {
					if _, err := h.db.Exec(`
						UPDATE workstations
						SET status = 'offline', updated_at = NOW()
						WHERE id = ? AND deleted_at IS NULL
					`, wsID); err != nil {
						logger.Printf("[AUTH] Failed to update workstation status on logout (ws=%d): %v", wsID, err)
					}
				} else if err != sql.ErrNoRows {
					logger.Printf("[AUTH] Failed to query workstation for collector on logout (collector=%d): %v", claims.CollectorID, err)
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me returns the current authenticated identity.
// Requires JWTAuth middleware; works for both admin and data_collector roles.
func (h *AuthHandler) Me(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if claims.Role == "admin" {
		c.JSON(http.StatusOK, gin.H{
			"collector_id":   nil,
			"operator_id":    "admin",
			"name":           "Administrator",
			"role":           "admin",
			"workstation_id": nil,
			"robot_id":       nil,
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

	var wsRow struct {
		ID      int64 `db:"id"`
		RobotID int64 `db:"robot_id"`
	}
	err := h.db.Get(&wsRow, `
		SELECT id, robot_id
		FROM workstations
		WHERE data_collector_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, claims.CollectorID)
	var workstationID *string
	var robotID *string
	if err != nil {
		if err != sql.ErrNoRows {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
	} else {
		ws := strconv.FormatInt(wsRow.ID, 10)
		rb := strconv.FormatInt(wsRow.RobotID, 10)
		workstationID = &ws
		robotID = &rb
	}

	c.JSON(http.StatusOK, gin.H{
		"collector_id":   claims.CollectorID,
		"operator_id":    row.OperatorID,
		"name":           row.Name,
		"role":           claims.Role,
		"workstation_id": workstationID,
		"robot_id":       robotID,
	})
}

// MeStationBreak sets the collector's workstation status to break (unless the workstation is offline).
// Requires RequireRole("data_collector") middleware.
func (h *AuthHandler) MeStationBreak(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var wsID int64
	err := h.db.Get(&wsID, `
		SELECT id
		FROM workstations
		WHERE data_collector_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, claims.CollectorID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workstation not assigned"})
		return
	}
	if err != nil {
		logger.Printf("[AUTH] MeStationBreak: failed to resolve workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	var cur string
	if err := h.db.Get(&cur, `SELECT status FROM workstations WHERE id = ? AND deleted_at IS NULL`, wsID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workstation not found"})
			return
		}
		logger.Printf("[AUTH] MeStationBreak: failed to read status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if cur == "offline" {
		c.JSON(http.StatusConflict, gin.H{"error": "workstation is offline"})
		return
	}
	if cur != "break" {
		if _, err := h.db.Exec(`
			UPDATE workstations
			SET status = 'break', updated_at = NOW()
			WHERE id = ? AND deleted_at IS NULL
		`, wsID); err != nil {
			logger.Printf("[AUTH] MeStationBreak: failed to update workstation %d: %v", wsID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workstation"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"workstation_id": strconv.FormatInt(wsID, 10),
		"status":         "break",
	})
}

// MeStationEndBreak sets the collector's workstation to active or inactive depending on whether
// an active batch exists. Does not override offline (returns 409).
// Requires RequireRole("data_collector") middleware.
func (h *AuthHandler) MeStationEndBreak(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var wsID int64
	err := h.db.Get(&wsID, `
		SELECT id
		FROM workstations
		WHERE data_collector_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, claims.CollectorID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workstation not assigned"})
		return
	}
	if err != nil {
		logger.Printf("[AUTH] MeStationEndBreak: failed to resolve workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	var cur string
	if err := h.db.Get(&cur, `SELECT status FROM workstations WHERE id = ? AND deleted_at IS NULL`, wsID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workstation not found"})
			return
		}
		logger.Printf("[AUTH] MeStationEndBreak: failed to read status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	if cur == "offline" {
		c.JSON(http.StatusConflict, gin.H{"error": "workstation is offline"})
		return
	}

	var hasActiveBatch bool
	if err := h.db.Get(&hasActiveBatch, `
		SELECT EXISTS(
			SELECT 1
			FROM batches
			WHERE workstation_id = ? AND status = 'active' AND deleted_at IS NULL
		)
	`, wsID); err != nil {
		logger.Printf("[AUTH] MeStationEndBreak: failed to query batches: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}
	newStatus := "inactive"
	if hasActiveBatch {
		newStatus = "active"
	}
	if _, err := h.db.Exec(`
		UPDATE workstations
		SET status = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, newStatus, wsID); err != nil {
		logger.Printf("[AUTH] MeStationEndBreak: failed to update workstation %d: %v", wsID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workstation"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"workstation_id": strconv.FormatInt(wsID, 10),
		"status":         newStatus,
	})
}

// syncWorkstationStatusOnLogin is a best-effort helper that syncs workstation
// status to active/inactive based on whether an active batch exists.
func (h *AuthHandler) syncWorkstationStatusOnLogin(collectorID int64) {
	var wsID int64
	if err := h.db.Get(&wsID, `
		SELECT id
		FROM workstations
		WHERE data_collector_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, collectorID); err != nil {
		if err != sql.ErrNoRows {
			logger.Printf("[AUTH] Failed to query workstation for collector on login (collector=%d): %v", collectorID, err)
		}
		return
	}
	var hasActiveBatch bool
	if err := h.db.Get(&hasActiveBatch, `
		SELECT EXISTS(
			SELECT 1
			FROM batches
			WHERE workstation_id = ? AND status = 'active' AND deleted_at IS NULL
		)
	`, wsID); err != nil {
		logger.Printf("[AUTH] Failed to query active batch for workstation on login (ws=%d): %v", wsID, err)
		return
	}
	newStatus := "inactive"
	if hasActiveBatch {
		newStatus = "active"
	}
	if _, err := h.db.Exec(`
		UPDATE workstations
		SET status = ?, updated_at = NOW()
		WHERE id = ? AND deleted_at IS NULL
	`, newStatus, wsID); err != nil {
		logger.Printf("[AUTH] Failed to update workstation status on login (ws=%d): %v", wsID, err)
	}
}
