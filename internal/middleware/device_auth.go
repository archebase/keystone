// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services/deviceauth"
)

// DevicePrincipalKey is the gin.Context key for the authenticated device.
const DevicePrincipalKey = "device_principal"

// DeviceAuth accepts exactly one persistent Device-Authorization credential
// or one temporary Device JWT in Authorization and attaches its principal.
func DeviceAuth(authenticator *deviceauth.Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		persistentHeader := strings.TrimSpace(c.GetHeader("Device-Authorization"))
		jwtHeader := strings.TrimSpace(c.GetHeader("Authorization"))
		if persistentHeader != "" && jwtHeader != "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":  "ambiguous_device_credentials",
				"error": "provide exactly one device credential",
			})
			return
		}
		if persistentHeader == "" && jwtHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":  "device_authentication_required",
				"error": "device authentication required",
			})
			return
		}

		var (
			principal deviceauth.Principal
			err       error
		)
		startedAt := time.Now()
		if persistentHeader != "" {
			token := parseDeviceAuthorizationHeader(persistentHeader)
			if token == "" {
				writeInvalidDeviceCredential(c)
				return
			}
			principal, err = authenticator.AuthenticatePersistent(c.Request.Context(), token)
		} else {
			token := parseBearerAuthorizationHeader(jwtHeader)
			if token == "" {
				writeInvalidDeviceCredential(c)
				return
			}
			principal, err = authenticator.AuthenticateJWT(c.Request.Context(), token)
		}
		if err != nil {
			if errors.Is(err, deviceauth.ErrInvalidCredential) {
				writeInvalidDeviceCredential(c)
				return
			}
			logger.Printf("[DEVICE] HTTP device authentication unavailable: method=%s path=%s elapsed_ms=%d error=%v", c.Request.Method, c.Request.URL.Path, time.Since(startedAt).Milliseconds(), err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "device authentication unavailable",
			})
			return
		}

		c.Set(DevicePrincipalKey, principal)
		c.Next()
	}
}

func writeInvalidDeviceCredential(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code":  "invalid_device_credential",
		"error": "invalid device credential",
	})
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

func parseBearerAuthorizationHeader(header string) string {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

// GetDevicePrincipal returns the identity attached by DeviceAuth.
func GetDevicePrincipal(c *gin.Context) *deviceauth.Principal {
	value, ok := c.Get(DevicePrincipalKey)
	if !ok {
		return nil
	}
	principal, ok := value.(deviceauth.Principal)
	if !ok {
		return nil
	}
	return &principal
}
