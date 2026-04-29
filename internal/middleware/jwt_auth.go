// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package middleware provides HTTP middleware for request authentication.
package middleware

import (
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"github.com/gin-gonic/gin"
)

// ClaimsKey is the gin.Context key used to store parsed JWT claims.
const ClaimsKey = "auth_claims"

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
