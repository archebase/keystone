// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestParseDataOpsEpisodeQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/data-ops/episodes?limit=20&offset=40&created_at_from=2026-06-01T00:00:00Z&created_at_to=2026-06-06T00:00:00Z&q=ep&qa_status=failed,pending_qa&sync_status=not_started,failed&scene_id=1,2&sop_id=9,10&robot_type_id=3&robot_device_id=robot-001,robot-002&collector_operator_id=op001&label=recalled_batch", nil)

	got, err := parseDataOpsEpisodeQuery(c)
	if err != nil {
		t.Fatalf("parseDataOpsEpisodeQuery returned error: %v", err)
	}
	if got.Pagination.Limit != 20 || got.Pagination.Offset != 40 {
		t.Fatalf("unexpected pagination: %+v", got.Pagination)
	}
	if !got.HasCreatedAtFrom || !got.HasCreatedAtTo || got.Keyword != "ep" || got.Label != "recalled_batch" {
		t.Fatalf("unexpected scalar filters: %+v", got)
	}
	if strings.Join(got.QAStatuses, ",") != "failed,pending_qa" {
		t.Fatalf("unexpected qa statuses: %#v", got.QAStatuses)
	}
	if strings.Join(got.SyncStatuses, ",") != "not_started,failed" {
		t.Fatalf("unexpected sync statuses: %#v", got.SyncStatuses)
	}
	if len(got.SceneIDs) != 2 || got.SceneIDs[0] != 1 || got.SceneIDs[1] != 2 {
		t.Fatalf("unexpected scene ids: %#v", got.SceneIDs)
	}
	if len(got.SOPIDs) != 2 || got.SOPIDs[0] != 9 || got.SOPIDs[1] != 10 {
		t.Fatalf("unexpected sop ids: %#v", got.SOPIDs)
	}
	if len(got.RobotTypeIDs) != 1 || got.RobotTypeIDs[0] != 3 {
		t.Fatalf("unexpected robot type ids: %#v", got.RobotTypeIDs)
	}
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
}

func TestDataOpsEpisodeWhereIncludesSOPFilter(t *testing.T) {
	sql, args := buildDataOpsEpisodeWhere(dataOpsEpisodeQuery{SOPIDs: []int64{9, 10}})
	if !strings.Contains(sql, "COALESCE(e.sop_id, t.sop_id) IN (?,?)") {
		t.Fatalf("SOP filter SQL should use episode/task SOP fallback: %s", sql)
	}
	if len(args) != 2 || args[0] != int64(9) || args[1] != int64(10) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsEpisodeListSQLIncludesSOPColumns(t *testing.T) {
	sql := dataOpsEpisodeListSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{
		"COALESCE(e.sop_id, t.sop_id) AS sop_id",
		"LEFT JOIN sops s ON s.id = COALESCE(e.sop_id, t.sop_id)",
		"CONCAT('SOP #', CAST(COALESCE(e.sop_id, t.sop_id) AS CHAR))",
		"ELSE CONCAT(s.slug, ' @ ', s.version)",
		"COALESCE(NULLIF(dc.name, ''), NULLIF(ws.collector_name, '')) AS collector_name",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("data ops SQL should include %q: %s", want, sql)
		}
	}
}

func TestDataOpsSyncStatusWhereSupportsNotStartedAndLatestStatus(t *testing.T) {
	sql, args := dataOpsSyncStatusWhere([]string{"not_started", "failed"})
	if !strings.Contains(sql, "NOT EXISTS") {
		t.Fatalf("sync status SQL should include not_started branch: %s", sql)
	}
	if !strings.Contains(sql, "MAX(sl2.id)") || !strings.Contains(sql, "sl_latest.status IN (?)") {
		t.Fatalf("sync status SQL should filter latest sync log status: %s", sql)
	}
	if len(args) != 1 || args[0] != "failed" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsLatestQueriesOnlyUsePageEpisodeIDs(t *testing.T) {
	qaSQL, qaArgs := dataOpsLatestQAChecksSQL([]int64{10, 20})
	if !strings.Contains(qaSQL, "WHERE episode_id IN (?,?)") {
		t.Fatalf("latest QA SQL should constrain page episode IDs: %s", qaSQL)
	}
	if len(qaArgs) != 2 {
		t.Fatalf("latest QA args = %#v", qaArgs)
	}

	syncSQL, syncArgs := dataOpsLatestSyncLogsSQL([]int64{10, 20})
	if !strings.Contains(syncSQL, "WHERE episode_id IN (?,?)") {
		t.Fatalf("latest sync SQL should constrain page episode IDs: %s", syncSQL)
	}
	if len(syncArgs) != 2 {
		t.Fatalf("latest sync args = %#v", syncArgs)
	}
}

func TestParseDataOpsBulkEpisodeFilters(t *testing.T) {
	got, err := parseDataOpsBulkEpisodeFilters(DataOpsBulkEpisodeFilters{
		CreatedAtFrom:       "2026-06-01T00:00:00Z",
		CreatedAtTo:         "2026-06-06T00:00:00Z",
		Keyword:             "ep",
		QAStatus:            "failed,pending_qa",
		SyncStatus:          "not_started,failed",
		SceneID:             "1,2",
		SOPID:               "9,10",
		RobotTypeID:         "3",
		RobotDeviceID:       "robot-001,robot-002",
		CollectorOperatorID: "op001",
		Label:               "recalled_batch",
		Limit:               "20",
		Offset:              "40",
	})
	if err != nil {
		t.Fatalf("parseDataOpsBulkEpisodeFilters returned error: %v", err)
	}
	if got.Pagination.Limit != 0 || got.Pagination.Offset != 0 {
		t.Fatalf("bulk filters should ignore pagination: %+v", got.Pagination)
	}
	if !got.HasCreatedAtFrom || !got.HasCreatedAtTo || got.Keyword != "ep" || got.Label != "recalled_batch" {
		t.Fatalf("unexpected scalar filters: %+v", got)
	}
	if strings.Join(got.QAStatuses, ",") != "failed,pending_qa" {
		t.Fatalf("unexpected qa statuses: %#v", got.QAStatuses)
	}
	if strings.Join(got.SyncStatuses, ",") != "not_started,failed" {
		t.Fatalf("unexpected sync statuses: %#v", got.SyncStatuses)
	}
	if len(got.SceneIDs) != 2 || got.SceneIDs[0] != 1 || got.SceneIDs[1] != 2 {
		t.Fatalf("unexpected scene ids: %#v", got.SceneIDs)
	}
	if len(got.SOPIDs) != 2 || got.SOPIDs[0] != 9 || got.SOPIDs[1] != 10 {
		t.Fatalf("unexpected sop ids: %#v", got.SOPIDs)
	}
	if len(got.RobotTypeIDs) != 1 || got.RobotTypeIDs[0] != 3 {
		t.Fatalf("unexpected robot type ids: %#v", got.RobotTypeIDs)
	}
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
}

func TestParseDataOpsBulkEpisodeFiltersDoesNotCapMultiValueCount(t *testing.T) {
	got, err := parseDataOpsBulkEpisodeFilters(DataOpsBulkEpisodeFilters{
		SceneID:       joinedNumberList(maxMultiValueFilterItems + 1),
		RobotDeviceID: joinedStringList("robot-", maxMultiValueFilterItems+1),
	})
	if err != nil {
		t.Fatalf("parseDataOpsBulkEpisodeFilters returned error: %v", err)
	}
	if len(got.SceneIDs) != maxMultiValueFilterItems+1 {
		t.Fatalf("scene id count = %d, want %d", len(got.SceneIDs), maxMultiValueFilterItems+1)
	}
	if len(got.RobotDeviceIDs) != maxMultiValueFilterItems+1 {
		t.Fatalf("robot device id count = %d, want %d", len(got.RobotDeviceIDs), maxMultiValueFilterItems+1)
	}
}

func TestParseDataOpsBulkEpisodeRequestConfirmGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"filters":{}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &DataOpsHandler{}
	if _, _, ok := h.parseBulkEpisodeActionRequest(c, true); ok {
		t.Fatal("bulk execute request without confirm should fail")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestParseDataOpsBulkEpisodeRequestPreviewDoesNotRequireConfirm(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/data-ops/episodes/bulk-qa/preview", bytes.NewBufferString(`{"filters":{"qa_status":"failed"}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &DataOpsHandler{}
	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		t.Fatal("bulk preview request should not require confirm")
	}
	if strings.Join(q.QAStatuses, ",") != "failed" {
		t.Fatalf("unexpected qa statuses: %#v", q.QAStatuses)
	}
}

func TestDataOpsEpisodeIDSnapshotSQLUsesDataOpsOrdering(t *testing.T) {
	sql := dataOpsEpisodeIDSnapshotSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{
		"SELECT e.id",
		"FROM episodes e",
		"ORDER BY e.created_at DESC, e.id DESC",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("ID snapshot SQL should include %q: %s", want, sql)
		}
	}
}

func TestDataOpsBulkPreviewSQLs(t *testing.T) {
	qaSQL := dataOpsBulkQAPreviewSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{"matched_count", "qa_running_count"} {
		if !strings.Contains(qaSQL, want) {
			t.Fatalf("QA preview SQL should include %q: %s", want, qaSQL)
		}
	}
	for _, removed := range []string{"protected_status_count", "needs_inspection", "inspector_approved", "rejected"} {
		if strings.Contains(qaSQL, removed) {
			t.Fatalf("QA preview SQL should not include %q: %s", removed, qaSQL)
		}
	}

	syncSQL := dataOpsBulkSyncPreviewSQL(dataOpsEpisodeBaseFromSQL()+dataOpsLatestSyncPreviewJoinSQL(), " WHERE e.deleted_at IS NULL")
	for _, want := range []string{"latest_sync", "eligible_count", "qa_not_approved_count", "already_synced_count", "sync_active_count"} {
		if !strings.Contains(syncSQL, want) {
			t.Fatalf("sync preview SQL should include %q: %s", want, syncSQL)
		}
	}
}

func TestPreviewBulkEpisodeSyncTreatsMissingSyncLogAsEligible(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	for id := int64(1); id <= 11; id++ {
		if _, err := db.Exec(`
			INSERT INTO episodes (id, episode_id, task_id, scene_id, qa_status, cloud_synced, deleted_at, created_at)
			VALUES (?, ?, 0, 0, 'approved', 0, NULL, '2026-06-01T00:00:00Z')
		`, id, "episode"); err != nil {
			t.Fatalf("insert episode %d: %v", id, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO sync_logs (id, episode_id, status)
		VALUES (1, 1, 'failed')
	`); err != nil {
		t.Fatalf("insert failed sync log: %v", err)
	}

	preview, err := h.previewBulkEpisodeSync(context.Background(), dataOpsEpisodeQuery{
		QAStatuses: []string{"approved"},
	})
	if err != nil {
		t.Fatalf("previewBulkEpisodeSync returned error: %v", err)
	}

	if preview.MatchedCount != 11 || preview.EligibleCount != 11 || preview.SkippedCount != 0 {
		t.Fatalf("preview counts = matched %d eligible %d skipped %d, want 11/11/0", preview.MatchedCount, preview.EligibleCount, preview.SkippedCount)
	}
	if len(preview.SkippedBreakdown) != 0 {
		t.Fatalf("unexpected skipped breakdown: %#v", preview.SkippedBreakdown)
	}
}

func TestBulkRunEpisodeQACreatesRunSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	release := make(chan struct{})
	h := &DataOpsHandler{db: db, qaRunner: controlledDataOpsQARunner{release: release}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-02T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 2, "2026-06-01T00:00:00Z")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Run     DataOpsBulkRunResponse `json:"run"`
		Message string                 `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if !strings.HasPrefix(got.Run.RunID, "bulk_qa_") {
		t.Fatalf("run_id = %q, want bulk_qa_ prefix", got.Run.RunID)
	}
	if got.Run.Action != "bulk_qa" || got.Run.Status != "queued" {
		t.Fatalf("run action/status = %s/%s, want bulk_qa/queued", got.Run.Action, got.Run.Status)
	}
	if got.Run.TotalCount != 2 || got.Run.ProcessedCount != 0 {
		t.Fatalf("run counts = total %d processed %d, want 2/0", got.Run.TotalCount, got.Run.ProcessedCount)
	}
	if got.Message != "2 episodes accepted for bulk QA" {
		t.Fatalf("message = %q", got.Message)
	}
	close(release)
	waitForBulkRunStatus(t, router, got.Run.RunID, dataOpsBulkRunStatusCompleted)
}

func TestBulkRunEpisodeQARejectsSecondActiveRun(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	release := make(chan struct{})
	h := &DataOpsHandler{db: db, qaRunner: controlledDataOpsQARunner{release: release}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-01T00:00:00Z")

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	firstReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(first, firstReq)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	secondReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(second, secondReq)

	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	var conflict struct {
		Error  string `json:"error"`
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if conflict.RunID != firstBody.Run.RunID || (conflict.Status != dataOpsBulkRunStatusQueued && conflict.Status != dataOpsBulkRunStatusRunning) {
		t.Fatalf("conflict = %+v, want run_id %s and active status", conflict, firstBody.Run.RunID)
	}
	close(release)
	waitForBulkRunStatus(t, router, firstBody.Run.RunID, dataOpsBulkRunStatusCompleted)
}

func TestGetBulkRunAndCurrentBulkRunReturnSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	release := make(chan struct{})
	h := &DataOpsHandler{db: db, qaRunner: controlledDataOpsQARunner{release: release}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-01T00:00:00Z")

	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	postReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body = %s", postRec.Code, postRec.Body.String())
	}
	var postBody struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &postBody); err != nil {
		t.Fatalf("decode post response: %v", err)
	}

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/"+postBody.Run.RunID, nil)
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var got DataOpsBulkRunResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.RunID != postBody.Run.RunID || got.TotalCount != 1 {
		t.Fatalf("snapshot = %+v, want run_id %s and total 1", got, postBody.Run.RunID)
	}

	currentRec := httptest.NewRecorder()
	currentReq := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/current?action=bulk_qa", nil)
	router.ServeHTTP(currentRec, currentReq)
	if currentRec.Code != http.StatusOK {
		t.Fatalf("current status = %d, body = %s", currentRec.Code, currentRec.Body.String())
	}
	var current DataOpsBulkRunResponse
	if err := json.Unmarshal(currentRec.Body.Bytes(), &current); err != nil {
		t.Fatalf("decode current response: %v", err)
	}
	if current.RunID != postBody.Run.RunID {
		t.Fatalf("current run_id = %s, want %s", current.RunID, postBody.Run.RunID)
	}
	close(release)
	waitForBulkRunStatus(t, router, postBody.Run.RunID, dataOpsBulkRunStatusCompleted)
}

func TestCurrentBulkRunReturnsNoContentWhenNoRunIsActive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db, qaRunner: scriptedDataOpsQARunner{}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/current?action=bulk_qa", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestBulkRunEpisodeQAWithNoMatchedEpisodesCompletesImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db, qaRunner: scriptedDataOpsQARunner{}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Run.Status != dataOpsBulkRunStatusCompleted || got.Run.TotalCount != 0 || got.Run.FinishedAt == nil {
		t.Fatalf("run = %+v, want completed empty run with finished_at", got.Run)
	}
}

func TestBulkRunEpisodeQAUpdatesRunProgressFromSuiteResults(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db, qaRunner: scriptedDataOpsQARunner{
		results: map[int64]*EpisodeQASuiteResponse{
			1: {EpisodeID: 1, Passed: true, Mode: qaRunModeManual},
			2: {EpisodeID: 2, Passed: false, Mode: qaRunModeManual},
		},
		errs: map[int64]error{
			3: errEpisodeQAAlreadyRunning,
			4: errors.New("s3 read failed"),
		},
	}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-04T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 2, "2026-06-03T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 3, "2026-06-02T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 4, "2026-06-01T00:00:00Z")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var accepted struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	got := waitForBulkRunStatus(t, router, accepted.Run.RunID, dataOpsBulkRunStatusCompleted)
	if got.TotalCount != 4 || got.ProcessedCount != 4 {
		t.Fatalf("total/processed = %d/%d, want 4/4", got.TotalCount, got.ProcessedCount)
	}
	if got.PassedCount != 1 || got.QAFailedCount != 1 || got.SkippedCount != 1 || got.ProcessingFailedCount != 1 {
		t.Fatalf("run counts = passed %d qa_failed %d skipped %d processing_failed %d, want 1/1/1/1", got.PassedCount, got.QAFailedCount, got.SkippedCount, got.ProcessingFailedCount)
	}
}

func TestInterruptActiveBulkQARunsMarksQueuedAndRunningRunsInterrupted(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	insertDataOpsBulkRunForTest(t, db, "bulk_qa_queued", dataOpsBulkRunStatusQueued)
	insertDataOpsBulkRunForTest(t, db, "bulk_qa_running", dataOpsBulkRunStatusRunning)
	insertDataOpsBulkRunForTest(t, db, "bulk_qa_completed", dataOpsBulkRunStatusCompleted)

	if err := h.InterruptActiveBulkQARuns(context.Background()); err != nil {
		t.Fatalf("InterruptActiveBulkQARuns returned error: %v", err)
	}

	for _, runID := range []string{"bulk_qa_queued", "bulk_qa_running"} {
		run, err := h.loadBulkRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("load %s: %v", runID, err)
		}
		if run.Status != dataOpsBulkRunStatusInterrupted || run.FinishedAt == nil {
			t.Fatalf("run %s = %+v, want interrupted with finished_at", runID, run)
		}
	}

	completed, err := h.loadBulkRun(context.Background(), "bulk_qa_completed")
	if err != nil {
		t.Fatalf("load completed run: %v", err)
	}
	if completed.Status != dataOpsBulkRunStatusCompleted {
		t.Fatalf("completed status = %s, want completed", completed.Status)
	}
}

func TestStreamBulkRunSendsSnapshotAndTerminalEventForCompletedRun(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkRunForTest(t, db, "bulk_qa_completed", dataOpsBulkRunStatusCompleted)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/bulk_qa_completed/stream", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: bulk_run_snapshot\n",
		`"run_id":"bulk_qa_completed"`,
		"event: bulk_run_completed\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body should contain %q, got:\n%s", want, body)
		}
	}
}

func TestStreamBulkRunClosesWhenRunningRunCompletes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	release := make(chan struct{})
	h := &DataOpsHandler{db: db, qaRunner: controlledDataOpsQARunner{release: release}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-01T00:00:00Z")

	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	postReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("post status = %d, body = %s", postRec.Code, postRec.Body.String())
	}
	var accepted struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode post response: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	streamRec := httptest.NewRecorder()
	streamReq := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/"+accepted.Run.RunID+"/stream", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(streamRec, streamReq)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after bulk run completed")
	}

	body := streamRec.Body.String()
	for _, want := range []string{
		"event: bulk_run_snapshot\n",
		"event: bulk_run_completed\n",
		`"processed_count":1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body should contain %q, got:\n%s", want, body)
		}
	}
}

func setupDataOpsBulkPreviewTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	})

	schema := []string{
		`CREATE TABLE episodes (
			id INTEGER PRIMARY KEY,
			episode_id TEXT NOT NULL,
			task_id INTEGER NOT NULL,
			scene_id INTEGER NOT NULL,
			workstation_id INTEGER,
			sop_id INTEGER,
			qa_status TEXT,
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			deleted_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			sop_id INTEGER,
			workstation_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE scenes (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			data_collector_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			robot_type_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE robot_types (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE data_collectors (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE sops (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY,
			episode_id INTEGER NOT NULL,
			status TEXT NOT NULL
		)`,
		`CREATE TABLE bulk_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL UNIQUE,
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			total_count INTEGER NOT NULL DEFAULT 0,
			processed_count INTEGER NOT NULL DEFAULT 0,
			passed_count INTEGER NOT NULL DEFAULT 0,
			qa_failed_count INTEGER NOT NULL DEFAULT 0,
			processing_failed_count INTEGER NOT NULL DEFAULT 0,
			skipped_count INTEGER NOT NULL DEFAULT 0,
			error_message TEXT,
			started_at TIMESTAMP NULL,
			finished_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func insertDataOpsBulkTestEpisode(t *testing.T, db *sqlx.DB, id int64, createdAt string) {
	t.Helper()

	if _, err := db.Exec(`
		INSERT INTO episodes (id, episode_id, task_id, scene_id, qa_status, cloud_synced, deleted_at, created_at)
		VALUES (?, ?, 0, 0, 'pending_qa', 0, NULL, ?)
	`, id, "episode", createdAt); err != nil {
		t.Fatalf("insert episode %d: %v", id, err)
	}
}

func insertDataOpsBulkRunForTest(t *testing.T, db *sqlx.DB, runID string, status string) {
	t.Helper()

	now := time.Date(2026, 6, 11, 7, 30, 12, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO bulk_runs (
			run_id, action, status, total_count, processed_count, passed_count,
			qa_failed_count, processing_failed_count, skipped_count, error_message,
			started_at, finished_at, created_at, updated_at
		)
		VALUES (?, 'bulk_qa', ?, 10, 0, 0, 0, 0, 0, '', NULL, NULL, ?, ?)
	`, runID, status, now, now); err != nil {
		t.Fatalf("insert bulk run %s: %v", runID, err)
	}
}

type controlledDataOpsQARunner struct {
	release <-chan struct{}
}

func (r controlledDataOpsQARunner) RunEpisodeQASuite(ctx context.Context, episodeID int64, mode QARunMode) (*EpisodeQASuiteResponse, error) {
	select {
	case <-r.release:
		return &EpisodeQASuiteResponse{EpisodeID: episodeID, Passed: true, Mode: mode}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type scriptedDataOpsQARunner struct {
	results map[int64]*EpisodeQASuiteResponse
	errs    map[int64]error
}

func (r scriptedDataOpsQARunner) RunEpisodeQASuite(_ context.Context, episodeID int64, _ QARunMode) (*EpisodeQASuiteResponse, error) {
	if err := r.errs[episodeID]; err != nil {
		return nil, err
	}
	if result := r.results[episodeID]; result != nil {
		return result, nil
	}
	return &EpisodeQASuiteResponse{EpisodeID: episodeID, Passed: true, Mode: qaRunModeManual}, nil
}

func waitForBulkRunStatus(t *testing.T, router http.Handler, runID string, status string) DataOpsBulkRunResponse {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	var last DataOpsBulkRunResponse
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/data-ops/bulk-runs/"+runID, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get run status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &last); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if last.Status == status {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("bulk run %s did not reach status %s, last snapshot = %+v", runID, status, last)
	return DataOpsBulkRunResponse{}
}
