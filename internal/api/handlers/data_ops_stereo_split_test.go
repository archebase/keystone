// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"archebase.com/keystone-edge/internal/services/stereosplit"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

func TestDataOpsStereoSplitRoutesStartAndReadCurrentDerivative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeDataOpsStereoSplitManager{
		derivative: stereosplit.Derivative{
			ID:                12,
			EpisodeID:         42,
			Kind:              stereosplit.Kind,
			Generation:        1,
			ProcessingStatus:  stereosplit.ProcessingQueued,
			QAStatus:          stereosplit.QANotStarted,
			OrbitDeleteStatus: stereosplit.DeleteNotRequired,
		},
		created:     true,
		imageConfig: stereosplit.ImageConfig{ID: 2, ImageRef: testHandlerImageDigest},
	}
	handler := NewDataOpsHandler(nil)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/data-ops/episodes/42/derivatives/stereo-split/process", nil))
	if response.Code != http.StatusAccepted || manager.startedEpisodeID != 42 {
		t.Fatalf("process response=%d body=%s started_episode=%d", response.Code, response.Body.String(), manager.startedEpisodeID)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/data-ops/episodes/42/derivatives/stereo-split", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"processing_status":"queued"`) {
		t.Fatalf("get response=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDataOpsStereoSplitGetReturnsSeparateDataAndProcessingDurations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	durationSec := 9.9
	processingDurationSec := 10.0
	manager := &fakeDataOpsStereoSplitManager{
		derivative: stereosplit.Derivative{
			ID:                    12,
			EpisodeID:             42,
			Kind:                  stereosplit.Kind,
			Generation:            1,
			ProcessingStatus:      stereosplit.ProcessingSucceeded,
			DurationSec:           &durationSec,
			ProcessingDurationSec: &processingDurationSec,
			QAStatus:              stereosplit.QAApproved,
			OrbitDeleteStatus:     stereosplit.DeleteCompleted,
		},
	}
	handler := NewDataOpsHandler(nil)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/data-ops/episodes/42/derivatives/stereo-split", nil))
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"duration_sec":9.9`) ||
		!strings.Contains(response.Body.String(), `"processing_duration_sec":10`) {
		t.Fatalf("get response=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDataOpsStereoSplitStartRejectsMissingImage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeDataOpsStereoSplitManager{}
	handler := NewDataOpsHandler(nil)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/data-ops/episodes/42/derivatives/stereo-split/process", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("process response=%d body=%s, want 503", response.Code, response.Body.String())
	}
	if manager.startedEpisodeID != 0 {
		t.Fatalf("Start() called for episode %d without an image", manager.startedEpisodeID)
	}
}

func TestDataOpsStereoSplitImageUpdateUsesExpectedRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeDataOpsStereoSplitManager{
		imageConfig: stereosplit.ImageConfig{ID: 3, ImageRef: testHandlerImageDigest},
	}
	handler := NewDataOpsHandler(nil)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/data-ops/processing-settings/stereo-split", strings.NewReader(`{
		"image_ref":"`+testHandlerImageDigest+`",
		"max_concurrent":3,
		"expected_revision_id":2
	}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || manager.expectedRevision != 2 || manager.updatedImage != testHandlerImageDigest || manager.updatedMaxConcurrent != 3 {
		t.Fatalf("update response=%d body=%s expected_revision=%d image=%q max_concurrent=%d", response.Code, response.Body.String(), manager.expectedRevision, manager.updatedImage, manager.updatedMaxConcurrent)
	}
}

func TestDataOpsStereoSplitSettingsRejectInvalidConcurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := &fakeDataOpsStereoSplitManager{
		imageConfig: stereosplit.ImageConfig{ID: 3, ImageRef: testHandlerImageDigest, MaxConcurrent: 1},
	}
	handler := NewDataOpsHandler(nil)
	handler.SetStereoSplitManager(manager)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	for _, maxConcurrent := range []int{0, 101} {
		response := httptest.NewRecorder()
		body := `{"image_ref":"` + testHandlerImageDigest + `","max_concurrent":` + strconv.Itoa(maxConcurrent) + `,"expected_revision_id":3}`
		request := httptest.NewRequest(http.MethodPut, "/data-ops/processing-settings/stereo-split", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("max_concurrent=%d response=%d body=%s, want 400", maxConcurrent, response.Code, response.Body.String())
		}
	}
	if manager.updatedMaxConcurrent != 0 {
		t.Fatalf("UpdateImageConfig() called with invalid max_concurrent=%d", manager.updatedMaxConcurrent)
	}
}

func TestDataOpsStereoSplitSettingsReturnUnavailableWithoutManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewDataOpsHandler(nil)
	router := gin.New()
	handler.RegisterRoutes(router.Group("/data-ops"))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/data-ops/processing-settings/stereo-split", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings response=%d body=%s, want 503", response.Code, response.Body.String())
	}
}

const testHandlerImageDigest = "ghcr.io/archebase/stereo-split@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeDataOpsStereoSplitManager struct {
	db                   *sqlx.DB
	derivative           stereosplit.Derivative
	created              bool
	imageConfig          stereosplit.ImageConfig
	startedEpisodeID     int64
	updatedImage         string
	updatedMaxConcurrent int
	expectedRevision     int64
	bulkAdmissions       map[int64]stereosplit.BulkAdmission
	canceledEpisodes     []int64
	admittedEpisodes     []int64
	admitCommitted       chan int64
	admitContinue        chan struct{}
	deferBulkResult      bool
	freezeBulkResult     bool
	mu                   sync.Mutex
}

func (f *fakeDataOpsStereoSplitManager) Start(_ context.Context, episodeID int64, _ string) (stereosplit.Derivative, bool, error) {
	f.startedEpisodeID = episodeID
	return f.derivative, f.created, nil
}

func (f *fakeDataOpsStereoSplitManager) Get(context.Context, int64) (stereosplit.Derivative, error) {
	return f.derivative, nil
}

func (f *fakeDataOpsStereoSplitManager) Retry(context.Context, int64, string) (stereosplit.Derivative, error) {
	return f.derivative, nil
}

func (f *fakeDataOpsStereoSplitManager) Cancel(_ context.Context, episodeID int64, _ string) (stereosplit.Derivative, error) {
	f.mu.Lock()
	f.canceledEpisodes = append(f.canceledEpisodes, episodeID)
	f.mu.Unlock()
	if f.db != nil {
		if _, err := f.db.Exec(`
			UPDATE bulk_run_items
			SET result_snapshot = ?, updated_at = ?
			WHERE episode_id = ? AND admission_status = ? AND result_snapshot IS NULL
		`, `{"generation":1,"processing_status":"canceled","qa_status":"not_started","orbit_delete_status":"not_required"}`,
			time.Now().UTC(), episodeID, stereosplit.BulkAdmissionAdmitted); err != nil {
			return stereosplit.Derivative{}, err
		}
	}
	derivative := f.derivative
	derivative.EpisodeID = episodeID
	derivative.ProcessingStatus = stereosplit.ProcessingCanceled
	return derivative, nil
}

func (f *fakeDataOpsStereoSplitManager) RetryQA(context.Context, int64, string) (stereosplit.Derivative, error) {
	return f.derivative, nil
}

func (f *fakeDataOpsStereoSplitManager) Logs(context.Context, int64) (string, error) {
	return "test logs", nil
}

func (f *fakeDataOpsStereoSplitManager) CurrentImageConfig(context.Context) (stereosplit.ImageConfig, error) {
	return f.imageConfig, nil
}

func (f *fakeDataOpsStereoSplitManager) UpdateImageConfig(_ context.Context, imageRef string, maxConcurrent int, resourceLimitsEnabled bool, expectedRevisionID int64, _ string) (stereosplit.ImageConfig, error) {
	f.updatedImage = imageRef
	f.updatedMaxConcurrent = maxConcurrent
	f.expectedRevision = expectedRevisionID
	f.imageConfig.ResourceLimitsEnabled = resourceLimitsEnabled
	return f.imageConfig, nil
}

func (f *fakeDataOpsStereoSplitManager) ListImageConfigHistory(context.Context, int, int) ([]stereosplit.ImageConfig, error) {
	return []stereosplit.ImageConfig{f.imageConfig}, nil
}

func (f *fakeDataOpsStereoSplitManager) FreezeBulkResultSnapshotsForRun(context.Context, string, int) (int, error) {
	if !f.freezeBulkResult {
		return 0, nil
	}
	return 1, nil
}
