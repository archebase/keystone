// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/services/deviceauth"
)

func TestDeviceAuthAcceptsPersistentCredential(t *testing.T) {
	db := newDeviceTokenAuthTestDB(t)
	token := "kda_v1_active-device-token"
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch) VALUES (101, 'device-101', 10, 'active', 1);
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash, revoked_at) VALUES (101, ?, NULL)
	`, deviceauth.HashPersistentToken(token)); err != nil {
		t.Fatalf("seed device credential: %v", err)
	}

	router := gin.New()
	router.GET("/device", DeviceAuth(newDeviceAuthenticator(db)), func(c *gin.Context) {
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

func TestDeviceAuthAcceptsTemporaryDeviceJWT(t *testing.T) {
	db := newDeviceTokenAuthTestDB(t)
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch)
		VALUES (101, 'device-101', 10, 'active', 1)
	`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	authenticator := newDeviceAuthenticator(db)
	token, _, err := authenticator.IssueJWT(deviceauth.Principal{
		RobotID: 101, DeviceID: "device-101", WorkspaceID: 10, AuthEpoch: 1,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("IssueJWT() error = %v", err)
	}

	router := gin.New()
	router.GET("/device", DeviceAuth(authenticator), func(c *gin.Context) {
		principal := GetDevicePrincipal(c)
		if principal == nil || principal.RobotID != 101 || principal.AuthEpoch != 1 {
			t.Fatalf("device principal = %#v", principal)
		}
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceAuthRejectsAmbiguousCredentials(t *testing.T) {
	router := gin.New()
	router.GET("/device", DeviceAuth(newDeviceAuthenticator(newDeviceTokenAuthTestDB(t))), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	request.Header.Set("Device-Authorization", "persistent-token")
	request.Header.Set("Authorization", "Bearer temporary-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "ambiguous_device_credentials") {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceAuthRejectsMissingCredential(t *testing.T) {
	router := gin.New()
	router.GET("/device", DeviceAuth(newDeviceAuthenticator(newDeviceTokenAuthTestDB(t))), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceAuthReturnsServiceUnavailableWhenAuthenticationDatabaseIsUnavailable(t *testing.T) {
	authenticator := newDeviceAuthenticator(nil)
	router := gin.New()
	router.GET("/device", DeviceAuth(authenticator), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/device", nil)
	request.Header.Set("Device-Authorization", "persistent-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestDeviceAuthRejectsRevokedPersistentCredential(t *testing.T) {
	db := newDeviceTokenAuthTestDB(t)
	token := "kda_v1_revoked-device-token"
	if _, err := db.Exec(`
		INSERT INTO robots (id, device_id, workspace_id, status, auth_epoch) VALUES (101, 'device-101', 10, 'active', 1);
		INSERT INTO ws_client_auth_tokens (robot_id, token_hash, revoked_at)
		VALUES (101, ?, CURRENT_TIMESTAMP)
	`, deviceauth.HashPersistentToken(token)); err != nil {
		t.Fatalf("seed revoked device credential: %v", err)
	}

	router := gin.New()
	router.GET("/device", DeviceAuth(newDeviceAuthenticator(db)), func(c *gin.Context) {
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
			auth_epoch INTEGER NOT NULL,
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

func newDeviceAuthenticator(db *sqlx.DB) *deviceauth.Authenticator {
	return deviceauth.New(db, deviceauth.Config{
		JWTSecret: "test-device-secret-at-least-32-bytes",
		JWTTTL:    15 * time.Minute,
	})
}
