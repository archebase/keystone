// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archebase.com/keystone-edge/internal/services/autosync"
	"github.com/gin-gonic/gin"
)

func TestAutoSyncSettingsHandlerUpdateAcceptsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeAutoSyncSettingsManager{
		current: autosync.Config{ID: 2, Enabled: true},
	}
	handler := NewAutoSyncSettingsHandler(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group(""))

	request := httptest.NewRequest(http.MethodPut, "/processing-settings/auto-sync", strings.NewReader(`{
		"enabled": false,
		"expected_revision_id": 2
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	if manager.updatedEnabled {
		t.Fatal("updated enabled = true, want false")
	}
	if manager.expectedRevision != 2 {
		t.Fatalf("expected revision = %d, want 2", manager.expectedRevision)
	}
}

func TestAutoSyncSettingsHandlerUpdateRejectsStaleRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeAutoSyncSettingsManager{
		current:   autosync.Config{ID: 3, Enabled: false},
		updateErr: fmt.Errorf("%w: current revision is 3", autosync.ErrConfigChanged),
	}
	handler := NewAutoSyncSettingsHandler(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group(""))

	request := httptest.NewRequest(http.MethodPut, "/processing-settings/auto-sync", strings.NewReader(`{
		"enabled": true,
		"expected_revision_id": 2
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"config_changed"`) {
		t.Fatalf("body = %s, want config_changed", response.Body.String())
	}
}

type fakeAutoSyncSettingsManager struct {
	current          autosync.Config
	updatedEnabled   bool
	expectedRevision int64
	updateErr        error
}

func (f *fakeAutoSyncSettingsManager) CurrentConfig(context.Context) (autosync.Config, error) {
	return f.current, nil
}

func (f *fakeAutoSyncSettingsManager) UpdateConfig(_ context.Context, enabled bool, expectedRevisionID int64, _ string) (autosync.Config, error) {
	f.updatedEnabled = enabled
	f.expectedRevision = expectedRevisionID
	if f.updateErr != nil {
		return autosync.Config{}, f.updateErr
	}
	f.current.ID++
	f.current.Enabled = enabled
	return f.current, nil
}
