// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"archebase.com/keystone-edge/internal/config"
	"github.com/gin-gonic/gin"
)

func TestIsDashboardDisplayToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.AuthConfig{DashboardDisplayToken: "display-secret"}

	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "valid display token", header: "Display display-secret", want: true},
		{name: "wrong token", header: "Display wrong", want: false},
		{name: "bearer is not display", header: "Bearer display-secret", want: false},
		{name: "empty configured token", header: "Display display-secret", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
			c.Request.Header.Set("Authorization", tt.header)
			testCfg := cfg
			if tt.name == "empty configured token" {
				testCfg = &config.AuthConfig{}
			}
			if got := IsDashboardDisplayToken(c, testCfg); got != tt.want {
				t.Fatalf("IsDashboardDisplayToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDashboardAuthAcceptsDisplayToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/dashboard", DashboardAuth(&config.AuthConfig{DashboardDisplayToken: "display-secret"}), func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil || claims.Role != "display" {
			t.Fatalf("claims = %#v, want display claims", claims)
		}
		if v, ok := c.Get(DashboardDisplayKey); !ok || v != true {
			t.Fatalf("dashboard display marker = %#v, want true", v)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Authorization", "Display display-secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNoContent, w.Body.String())
	}
}
