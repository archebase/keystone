// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"errors"
	"net/http"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// DevicePrincipalKey is the gin.Context key for the authenticated device.
const DevicePrincipalKey = "device_principal"

// DeviceTokenAuth validates Device-Authorization and attaches the active
// device identity to the request context.
func DeviceTokenAuth(db *sqlx.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := parseDeviceAuthorizationHeader(c.GetHeader("Device-Authorization"))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":  "device_authentication_required",
				"error": "device authentication required",
			})
			return
		}

		principal, err := services.AuthenticateDeviceAuthToken(c.Request.Context(), db, token)
		if err != nil {
			if errors.Is(err, services.ErrInvalidDeviceAuthToken) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"code":  "invalid_device_credential",
					"error": "invalid device credential",
				})
				return
			}
			logger.Printf("[DEVICE] HTTP device authentication failed: %v", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "device authentication unavailable",
			})
			return
		}

		c.Set(DevicePrincipalKey, principal)
		c.Next()
	}
}

func parseDeviceAuthorizationHeader(header string) string {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) == 1 {
		return parts[0]
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

// GetDevicePrincipal returns the identity attached by DeviceTokenAuth.
func GetDevicePrincipal(c *gin.Context) *services.DevicePrincipal {
	value, ok := c.Get(DevicePrincipalKey)
	if !ok {
		return nil
	}
	principal, ok := value.(services.DevicePrincipal)
	if !ok {
		return nil
	}
	return &principal
}
