// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDeviceTokenAuthAcceptsActiveCredential(t *testing.T) {
	db := newDeviceTokenAuthTestDB(t)
	token := "kda_v1_active-device-token"
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, 'device-101', 10, 'active');
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash, revoked_at) VALUES (101, ?, NULL)
	`, services.HashDeviceAuthToken(token)); err != nil {
		t.Fatalf("seed device credential: %v", err)
	}

	router := gin.New()
	router.GET("/device", DeviceTokenAuth(db), func(c *gin.Context) {
		principal := GetDevicePrincipal(c)
		if principal == nil {
			t.Fatal("device principal is missing")
		}
		if principal.RobotID != 101 || principal.DeviceID != "device-101" || principal.WorkspaceID != 10 {
			t.Fatalf("device principal = %#v", principal)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	request.Header.Set("Device-Authorization", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceTokenAuthRejectsMissingCredential(t *testing.T) {
	router := gin.New()
	router.GET("/device", DeviceTokenAuth(newDeviceTokenAuthTestDB(t)), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceTokenAuthRejectsRevokedCredential(t *testing.T) {
	db := newDeviceTokenAuthTestDB(t)
	token := "kda_v1_revoked-device-token"
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status) VALUES (101, 'device-101', 10, 'active');
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash, revoked_at)
		VALUES (101, ?, CURRENT_TIMESTAMP)
	`, services.HashDeviceAuthToken(token)); err != nil {
		t.Fatalf("seed revoked device credential: %v", err)
	}

	router := gin.New()
	router.GET("/device", DeviceTokenAuth(db), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	request.Header.Set("Device-Authorization", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func newDeviceTokenAuthTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			device_id TEXT NOT NULL,
			workspace_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			deleted_at TIMESTAMP
		)`,
		`CREATE TABLE ws_client_auth_tokens (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			robot_id INTEGER NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			revoked_at TIMESTAMP
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create device auth schema: %v", err)
		}
	}
	return db
}
