// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
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

func TestJWTAuthRejectsSupersededWorkstationSession(t *testing.T) {
	db := newTestJWTAuthDB(t, false)
	defer db.Close()
	cfg := &config.AuthConfig{
		JWTSecret:      "test-jwt-secret-at-least-32-bytes-long",
		Issuer:         "keystone-test",
		JWTExpiryHours: 24,
	}
	token, err := auth.GenerateToken(auth.NewCollectorWorkstationClaims(7, "dc01", 11, 101, 10), cfg)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	router := gin.New()
	router.GET("/protected", JWTAuth(cfg, db), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestJWTAuthRejectsCollectorSessionWithoutWorkstation(t *testing.T) {
	db := newTestJWTAuthDB(t, true)
	defer db.Close()
	cfg := testJWTAuthConfig()
	token, err := auth.GenerateToken(auth.NewCollectorClaims(7, "dc01"), cfg)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	router := gin.New()
	router.GET("/protected", JWTAuth(cfg, db), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestDashboardAuthRejectsSupersededWorkstationSession(t *testing.T) {
	db := newTestJWTAuthDB(t, false)
	defer db.Close()
	cfg := testJWTAuthConfig()
	token, err := auth.GenerateToken(auth.NewCollectorWorkstationClaims(7, "dc01", 11, 101, 10), cfg)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	router := gin.New()
	router.GET("/dashboard", DashboardAuth(cfg, db), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func newTestJWTAuthDB(t *testing.T, current bool) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			data_collector_id INTEGER NOT NULL,
			is_current BOOLEAN NOT NULL,
			deleted_at TEXT
		)
	`); err != nil {
		db.Close()
		t.Fatalf("create workstations: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workstations (id, data_collector_id, is_current, deleted_at)
		VALUES (11, 7, ?, NULL)
	`, current); err != nil {
		db.Close()
		t.Fatalf("seed workstations: %v", err)
	}
	return db
}

func testJWTAuthConfig() *config.AuthConfig {
	return &config.AuthConfig{
		JWTSecret:      "test-jwt-secret-at-least-32-bytes-long",
		Issuer:         "keystone-test",
		JWTExpiryHours: 24,
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
