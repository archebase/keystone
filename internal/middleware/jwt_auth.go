// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package middleware provides HTTP middleware for request authentication.
package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"github.com/gin-gonic/gin"
)

// ClaimsKey is the gin.Context key used to store parsed JWT claims.
const ClaimsKey = "auth_claims"

// DashboardDisplayKey is set when a request is authenticated by the production
// dashboard display token rather than a normal user JWT.
const DashboardDisplayKey = "dashboard_display_auth"

// JWTAuth validates JWT tokens.
// Header: Authorization: Bearer <jwt_token>
func JWTAuth(cfg *config.AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		claims, err := auth.ParseToken(parts[1], cfg)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// DashboardAuth allows a normal Bearer JWT or the optional production dashboard
// display token. It must only be mounted on production-dashboard read routes.
func DashboardAuth(cfg *config.AuthConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if IsDashboardDisplayRequest(c) {
			if !IsDashboardDisplayToken(c, cfg) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid display token"})
				return
			}
			c.Set(ClaimsKey, &auth.Claims{Role: "display"})
			c.Set(DashboardDisplayKey, true)
			c.Next()
			return
		}

		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		claims, err := auth.ParseToken(parts[1], cfg)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// IsDashboardDisplayRequest reports whether the request uses the Display auth
// scheme. It does not validate the token value.
func IsDashboardDisplayRequest(c *gin.Context) bool {
	scheme, _, ok := splitAuthorization(c.GetHeader("Authorization"))
	return ok && strings.EqualFold(scheme, "Display")
}

// IsDashboardDisplayToken validates Authorization: Display <token> against the
// optional KEYSTONE_DASHBOARD_DISPLAY_TOKEN value.
func IsDashboardDisplayToken(c *gin.Context, cfg *config.AuthConfig) bool {
	if cfg == nil {
		return false
	}
	scheme, token, ok := splitAuthorization(c.GetHeader("Authorization"))
	if !ok || !strings.EqualFold(scheme, "Display") {
		return false
	}
	expected := strings.TrimSpace(cfg.DashboardDisplayToken)
	token = strings.TrimSpace(token)
	if expected == "" || token == "" {
		return false
	}
	expectedHash := sha256.Sum256([]byte(expected))
	tokenHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(expectedHash[:], tokenHash[:]) == 1
}

func splitAuthorization(authHeader string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(authHeader), " ", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	scheme := strings.TrimSpace(parts[0])
	token := strings.TrimSpace(parts[1])
	if scheme == "" || token == "" {
		return "", "", false
	}
	return scheme, token, true
}

// GetClaims returns JWT claims previously stored in the gin.Context by JWTAuth.
func GetClaims(c *gin.Context) *auth.Claims {
	if v, ok := c.Get(ClaimsKey); ok {
		return v.(*auth.Claims)
	}
	return nil
}

// RequireRole returns a middleware that allows only requests whose JWT claims
// carry one of the specified roles. JWTAuth must run before this middleware.
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		allowed[r] = struct{}{}
	}
	return func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		if _, ok := allowed[claims.Role]; !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			return
		}
		c.Next()
	}
}

// RequireAnyRole is an alias for RequireRole with multiple roles — provided for
// readability at call sites where two or more roles are explicitly enumerated.
func RequireAnyRole(roles ...string) gin.HandlerFunc {
	return RequireRole(roles...)
}
