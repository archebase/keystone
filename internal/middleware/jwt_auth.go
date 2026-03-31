// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"github.com/gin-gonic/gin"
)

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

func GetClaims(c *gin.Context) *auth.Claims {
	if v, ok := c.Get(ClaimsKey); ok {
		return v.(*auth.Claims)
	}
	return nil
}

