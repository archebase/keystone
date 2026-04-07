// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
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

// CollectorLoginRequest is the request body for collector login.
type CollectorLoginRequest struct {
	OperatorID string `json:"operator_id" binding:"required"`
	Password   string `json:"password" binding:"required"` // #nosec G117 -- request DTO intentionally contains password
}

// CollectorLoginResponse is the response body returned after a successful collector login.
type CollectorLoginResponse struct {
	AccessToken string `json:"access_token"` // #nosec G117 -- response DTO intentionally returns access token
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Collector   struct {
		ID         string `json:"id"`
		OperatorID string `json:"operator_id"`
		Name       string `json:"name"`
	} `json:"collector"`
}

type collectorAuthRow struct {
	ID           int64          `db:"id"`
	Name         string         `db:"name"`
	OperatorID   string         `db:"operator_id"`
	PasswordHash sql.NullString `db:"password_hash"`
}

// RegisterRoutes registers auth endpoints under the provided router group.
func (h *AuthHandler) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/auth/login", h.LoginCollector)
	r.POST("/auth/logout", h.Logout)
	r.GET("/auth/me", h.Me)
	r.POST("/auth/me/station/break", h.MeStationBreak)
	r.POST("/auth/me/station/end-break", h.MeStationEndBreak)
}

// requireCollectorClaims parses Bearer JWT and returns collector claims, or writes 401 and returns false.
func (h *AuthHandler) requireCollectorClaims(c *gin.Context) (*auth.Claims, bool) {
	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
		return nil, false
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
		return nil, false
	}
	claims, err := auth.ParseToken(parts[1], h.cfg)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return nil, false
	}
	return claims, true
}

// LoginCollector authenticates data collector and returns JWT access token.
func (h *AuthHandler) LoginCollector(c *gin.Context) {
	var req CollectorLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.OperatorID = strings.TrimSpace(req.OperatorID)
	if req.OperatorID == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id and password are required"})
		return
	}

	var row collectorAuthRow
	err := h.db.Get(&row, `
		SELECT id, name, operator_id, password_hash
		FROM data_collectors
		WHERE operator_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, req.OperatorID)
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

	// On successful login, set workstation status to active/inactive based on
	// whether there is an active batch for this workstation.
	//
	// Best-effort only: if the lookup/update fails, do not block login.
	var wsID int64
	if err := h.db.Get(&wsID, `
		SELECT id
		FROM workstations
		WHERE data_collector_id = ? AND deleted_at IS NULL
		LIMIT 1
	`, row.ID); err == nil {
		var hasActiveBatch bool
		if err := h.db.Get(&hasActiveBatch, `
			SELECT EXISTS(
				SELECT 1
				FROM batches
				WHERE workstation_id = ? AND status = 'active' AND deleted_at IS NULL
			)
		`, wsID); err == nil {
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
		} else {
			logger.Printf("[AUTH] Failed to query active batch for workstation on login (ws=%d): %v", wsID, err)
		}
	} else if err != sql.ErrNoRows {
		logger.Printf("[AUTH] Failed to query workstation for collector on login (collector=%d): %v", row.ID, err)
	}

	claims := auth.NewCollectorClaims(row.ID, row.OperatorID)
	token, err := auth.GenerateToken(claims, h.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	var resp CollectorLoginResponse
	resp.AccessToken = token
	resp.TokenType = "Bearer"
	resp.ExpiresIn = h.cfg.JWTExpiryHours * 3600
	resp.Collector.ID = strconv.FormatInt(row.ID, 10)
	resp.Collector.OperatorID = row.OperatorID
	resp.Collector.Name = row.Name

	c.JSON(http.StatusOK, resp)
}

// Logout acknowledges logout. The client discards the token; if a valid Bearer
// token is present, the handler best-effort sets the workstation status to offline.
func (h *AuthHandler) Logout(c *gin.Context) {
	// MVP logout: client drops token.
	//
	// Additionally, best-effort update workstation status to offline for the
	// authenticated data collector.
	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && strings.TrimSpace(parts[1]) != "" {
			if claims, err := auth.ParseToken(parts[1], h.cfg); err == nil {
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

// Me returns the current authenticated collector identity.
func (h *AuthHandler) Me(c *gin.Context) {
	claims, ok := h.requireCollectorClaims(c)
	if !ok {
		return
	}

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
func (h *AuthHandler) MeStationBreak(c *gin.Context) {
	claims, ok := h.requireCollectorClaims(c)
	if !ok {
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
// an active batch exists (same rule as login). Does not override offline (returns 409).
func (h *AuthHandler) MeStationEndBreak(c *gin.Context) {
	claims, ok := h.requireCollectorClaims(c)
	if !ok {
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
