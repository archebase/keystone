// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

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

// Logout ends the current session. In this MVP it is a no-op on the server side.
func (h *AuthHandler) Logout(c *gin.Context) {
	// MVP logout: client drops token. Keep endpoint for symmetry.
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Me returns the current authenticated collector identity.
func (h *AuthHandler) Me(c *gin.Context) {
	authHeader := c.GetHeader("Authorization")
	if strings.TrimSpace(authHeader) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
		return
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
		return
	}

	claims, err := auth.ParseToken(parts[1], h.cfg)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
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

	c.JSON(http.StatusOK, gin.H{
		"collector_id": claims.CollectorID,
		"operator_id":  row.OperatorID,
		"name":         row.Name,
		"role":         claims.Role,
	})
}
