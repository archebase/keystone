// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/storage/s3"
)

func TestParseDataOpsEpisodeQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/data-ops/episodes?limit=20&offset=40&workspace_id=0&created_at_from=2026-06-01T00:00:00Z&created_at_to=2026-06-06T00:00:00Z&q=ep&qa_status=failed,pending_qa&sync_status=not_started,failed&robot_device_id=robot-001,robot-002&collector_operator_id=op001&dc_project_id=13&dc_project_name=Project&dc_task_id=14&dc_task_name=Task&label=recalled_batch", nil)

	got, err := parseDataOpsEpisodeQuery(c)
	if err != nil {
		t.Fatalf("parseDataOpsEpisodeQuery returned error: %v", err)
	}
	if got.Pagination.Limit != 20 || got.Pagination.Offset != 40 {
		t.Fatalf("unexpected pagination: %+v", got.Pagination)
	}
	if len(got.WorkspaceIDs) != 1 || got.WorkspaceIDs[0] != 0 {
		t.Fatalf("unexpected workspace ids: %#v", got.WorkspaceIDs)
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
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
	if len(got.DCProjectIDs) != 1 || got.DCProjectIDs[0] != 13 || got.DCProjectName != "Project" {
		t.Fatalf("unexpected project filters: %+v", got)
	}
	if len(got.DCTaskIDs) != 1 || got.DCTaskIDs[0] != 14 || got.DCTaskName != "Task" {
		t.Fatalf("unexpected task filters: %+v", got)
	}
}

func TestDataOpsEpisodeWhereIncludesWorkspaceFilter(t *testing.T) {
	sql, args := buildDataOpsEpisodeWhere(dataOpsEpisodeQuery{WorkspaceIDs: []int64{0, 12}})
	if !strings.Contains(sql, "COALESCE(t.organization_id, ws.workspace_id) IN (?,?)") {
		t.Fatalf("workspace filter SQL should use task/workstation fallback: %s", sql)
	}
	if len(args) != 2 || args[0] != int64(0) || args[1] != int64(12) {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsEpisodeWhereIncludesProjectAndTaskFilters(t *testing.T) {
	sql, args := buildDataOpsEpisodeWhere(dataOpsEpisodeQuery{
		DCProjectIDs:  []int64{13},
		DCTaskIDs:     []int64{14},
		DCProjectName: "Project",
		DCTaskName:    "Task",
	})
	for _, want := range []string{
		"dp.dc_project_id IN (?)",
		"dp.dc_task_id IN (?)",
		"dp.dc_project_name LIKE ?",
		"dp.dc_task_name LIKE ?",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("project/task filter SQL should include %q: %s", want, sql)
		}
	}
	if len(args) != 4 || args[0] != int64(13) || args[1] != int64(14) || args[2] != "%Project%" || args[3] != "%Task%" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestDataOpsHilbertSyncRejectsDefaultWorkspaceScope(t *testing.T) {
	if dataOpsHilbertSyncAllowed(dataOpsEpisodeQuery{}) {
		t.Fatal("bulk Hilbert sync must require an explicit Workspace")
	}
	if dataOpsHilbertSyncAllowed(dataOpsEpisodeQuery{WorkspaceIDs: []int64{0}}) {
		t.Fatal("default Workspace must not allow Hilbert sync")
	}
	if dataOpsHilbertSyncAllowed(dataOpsEpisodeQuery{WorkspaceIDs: []int64{0, 12}}) {
		t.Fatal("mixed scope containing default Workspace must not allow Hilbert sync")
	}
	if dataOpsHilbertSyncAllowed(dataOpsEpisodeQuery{WorkspaceIDs: []int64{12, 13}}) {
		t.Fatal("bulk Hilbert sync must not span multiple Workspaces")
	}
	if !dataOpsHilbertSyncAllowed(dataOpsEpisodeQuery{WorkspaceIDs: []int64{12}}) {
		t.Fatal("Hilbert Workspace should allow sync")
	}
}

func TestDataOpsEpisodeListSQLUsesCurrentProductionMetadata(t *testing.T) {
	sql := dataOpsEpisodeListSQL(dataOpsEpisodeBaseFromSQL(), " WHERE e.deleted_at IS NULL")
	for _, current := range []string{"LEFT JOIN tasks", "LEFT JOIN dc_plan", "LEFT JOIN workstations", "LEFT JOIN robots", "LEFT JOIN data_collectors"} {
		if !strings.Contains(sql, current) {
			t.Fatalf("data ops SQL should include %q: %s", current, sql)
		}
	}
	if !strings.Contains(sql, "dp.dc_project_id") || !strings.Contains(sql, "dp.dc_project_name") {
		t.Fatalf("data ops SQL should select dc project fields: %s", sql)
	}
	if !strings.Contains(sql, "dp.dc_task_id") || !strings.Contains(sql, "dp.dc_task_name") {
		t.Fatalf("data ops SQL should select dc task fields: %s", sql)
	}
	if !strings.Contains(sql, "r.metadata AS robot_metadata") {
		t.Fatalf("data ops SQL should select robot metadata for device names: %s", sql)
	}
}

func TestDataOpsEpisodeItemIncludesTaskAndRobotDisplayNames(t *testing.T) {
	item := dataOpsEpisodeItemFromRow(dataOpsEpisodeRow{
		ID:            1,
		EpisodeID:     "episode-1",
		TaskID:        10,
		DCProjectID:   sql.NullInt64{Int64: 13, Valid: true},
		DCProjectName: sql.NullString{String: "Project One", Valid: true},
		DCTaskID:      sql.NullInt64{Int64: 14, Valid: true},
		DCTaskName:    sql.NullString{String: "Task One", Valid: true},
		RobotDeviceID: sql.NullString{String: "robot-001", Valid: true},
		RobotMetadata: sql.NullString{String: `{"hilbert_dc_device_name":"Device One"}`, Valid: true},
		QAStatus:      "pending_qa",
		CreatedAt:     time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
	})
	if item.DCTaskID == nil || *item.DCTaskID != 14 || item.DCTaskName == nil || *item.DCTaskName != "Task One" {
		t.Fatalf("task fields=%v/%v want 14/Task One", item.DCTaskID, item.DCTaskName)
	}
	if item.DCProjectID == nil || *item.DCProjectID != 13 || item.DCProjectName == nil || *item.DCProjectName != "Project One" {
		t.Fatalf("project fields=%v/%v want 13/Project One", item.DCProjectID, item.DCProjectName)
	}
	if item.RobotDeviceName == nil || *item.RobotDeviceName != "Device One" {
		t.Fatalf("RobotDeviceName=%v want Device One", item.RobotDeviceName)
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
		WorkspaceID:         "12",
		CreatedAtFrom:       "2026-06-01T00:00:00Z",
		CreatedAtTo:         "2026-06-06T00:00:00Z",
		Keyword:             "ep",
		QAStatus:            "failed,pending_qa",
		SyncStatus:          "not_started,failed",
		RobotDeviceID:       "robot-001,robot-002",
		CollectorOperatorID: "op001",
		DCProjectID:         "13",
		DCProjectName:       "Project",
		DCTaskID:            "14",
		DCTaskName:          "Task",
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
	if len(got.WorkspaceIDs) != 1 || got.WorkspaceIDs[0] != 12 {
		t.Fatalf("unexpected workspace ids: %#v", got.WorkspaceIDs)
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
	if strings.Join(got.RobotDeviceIDs, ",") != "robot-001,robot-002" || strings.Join(got.CollectorOperatorIDs, ",") != "op001" {
		t.Fatalf("unexpected string filters: %+v", got)
	}
	if len(got.DCProjectIDs) != 1 || got.DCProjectIDs[0] != 13 || got.DCProjectName != "Project" {
		t.Fatalf("unexpected project filters: %+v", got)
	}
	if len(got.DCTaskIDs) != 1 || got.DCTaskIDs[0] != 14 || got.DCTaskName != "Task" {
		t.Fatalf("unexpected task filters: %+v", got)
	}
}

func TestParseDataOpsBulkEpisodeFiltersDoesNotCapMultiValueCount(t *testing.T) {
	got, err := parseDataOpsBulkEpisodeFilters(DataOpsBulkEpisodeFilters{
		RobotDeviceID: joinedStringList("robot-", maxMultiValueFilterItems+1),
	})
	if err != nil {
		t.Fatalf("parseDataOpsBulkEpisodeFilters returned error: %v", err)
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
	for _, want := range []string{"matched_count", "qa_running_count", "cloud_synced_count", "sync_active_count"} {
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

func TestPreviewBulkEpisodeQASkipsCloudSyncedEpisodes(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-03T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 2, "2026-06-02T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 3, "2026-06-01T00:00:00Z")
	insertDataOpsBulkTestEpisode(t, db, 4, "2026-05-31T00:00:00Z")
	if _, err := db.Exec(`UPDATE episodes SET cloud_synced = TRUE, qa_status = 'approved' WHERE id = 2`); err != nil {
		t.Fatalf("mark episode synced: %v", err)
	}
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'qa_running' WHERE id = 3`); err != nil {
		t.Fatalf("mark episode QA running: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sync_logs (episode_id, status) VALUES (4, 'pending')`); err != nil {
		t.Fatalf("insert active sync log: %v", err)
	}

	preview, err := h.previewBulkEpisodeQA(context.Background(), dataOpsEpisodeQuery{})
	if err != nil {
		t.Fatalf("previewBulkEpisodeQA returned error: %v", err)
	}

	if preview.MatchedCount != 4 || preview.EligibleCount != 1 || preview.SkippedCount != 3 {
		t.Fatalf("preview counts = matched %d eligible %d skipped %d, want 4/1/3",
			preview.MatchedCount, preview.EligibleCount, preview.SkippedCount)
	}
	if len(preview.SkippedBreakdown) != 3 ||
		preview.SkippedBreakdown[0].Reason != "qa_running" || preview.SkippedBreakdown[0].Count != 1 ||
		preview.SkippedBreakdown[1].Reason != "already_synced" || preview.SkippedBreakdown[1].Count != 1 ||
		preview.SkippedBreakdown[2].Reason != "sync_active" || preview.SkippedBreakdown[2].Count != 1 {
		t.Fatalf("unexpected skipped breakdown: %#v", preview.SkippedBreakdown)
	}
}

func TestPreviewBulkEpisodeSyncTreatsMissingSyncLogAsEligible(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	for id := int64(1); id <= 11; id++ {
		if _, err := db.Exec(`
			INSERT INTO episodes (id, episode_id, task_id, qa_status, cloud_synced, deleted_at, created_at)
			VALUES (?, ?, 0, 'approved', 0, NULL, '2026-06-01T00:00:00Z')
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

func TestPreviewBulkEpisodeMP4SkipsFailedQA(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	insertDataOpsBulkTestEpisode(t, db, 1, "2026-06-10T10:00:00Z")
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'failed' WHERE id = 1`); err != nil {
		t.Fatalf("update failed episode: %v", err)
	}
	insertDataOpsBulkTestEpisode(t, db, 2, "2026-06-11T10:00:00Z")
	if _, err := db.Exec(`UPDATE episodes SET qa_status = 'approved' WHERE id = 2`); err != nil {
		t.Fatalf("update approved episode: %v", err)
	}

	preview, err := h.previewBulkEpisodeMP4(context.Background(), dataOpsEpisodeQuery{})
	if err != nil {
		t.Fatalf("previewBulkEpisodeMP4 returned error: %v", err)
	}

	if preview.MatchedCount != 2 || preview.EligibleCount != 1 || preview.SkippedCount != 1 {
		t.Fatalf("preview counts = matched %d eligible %d skipped %d, want 2/1/1", preview.MatchedCount, preview.EligibleCount, preview.SkippedCount)
	}
	if len(preview.SkippedBreakdown) != 1 || preview.SkippedBreakdown[0].Reason != "auto_qa_failed" || preview.SkippedBreakdown[0].Count != 1 {
		t.Fatalf("unexpected skipped breakdown: %#v", preview.SkippedBreakdown)
	}
}

func TestFindBulkMP4OutputAcceptsCodecSubdirectory(t *testing.T) {
	outputDir := t.TempDir()
	codecDir := filepath.Join(outputDir, "egolite")
	if err := os.MkdirAll(codecDir, 0o755); err != nil {
		t.Fatalf("mkdir codec dir: %v", err)
	}
	want := filepath.Join(codecDir, "episode_1.mp4")
	if err := os.WriteFile(want, []byte("mp4"), 0o644); err != nil {
		t.Fatalf("write mp4: %v", err)
	}

	got, err := findBulkMP4Output(outputDir, filepath.Join(t.TempDir(), "input.mcap"))
	if err != nil {
		t.Fatalf("findBulkMP4Output returned error: %v", err)
	}
	if got != want {
		t.Fatalf("findBulkMP4Output = %q, want %q", got, want)
	}
}

func TestResolveDataOpsMP4LocationUsesEpisodeStorageMetadata(t *testing.T) {
	const objectName = "device-uploads/5/capture_20260728T053142Z_5e20cc0b/0bfc08b5-710b-495c-a890-123c3692bc02/capture.mcap"
	row := dataOpsBulkMP4EpisodeRow{
		ID:       11,
		McapPath: objectName,
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"source":"dgwcompat","object_store_backend":"volcengine_tos","bucket":"archebase-keystone-device-upload-2116584179","object_key":"` + objectName + `"}`,
		},
	}

	bucket, gotObjectName, ok := resolveDataOpsMP4Location("edge-factory-archebase", row)
	if !ok {
		t.Fatal("resolveDataOpsMP4Location() ok = false, want true")
	}
	if bucket != "archebase-keystone-device-upload-2116584179" {
		t.Fatalf("bucket = %q, want episode metadata bucket", bucket)
	}
	if gotObjectName != objectName {
		t.Fatalf("objectName = %q, want %q", gotObjectName, objectName)
	}
}

func TestResolveDataOpsMP4LocationKeepsAxonTransferOnMinIO(t *testing.T) {
	row := dataOpsBulkMP4EpisodeRow{
		ID:       12,
		McapPath: "minio-bucket/axon-transfer/capture.mcap",
		Metadata: sql.NullString{
			Valid:  true,
			String: `{"asset_id":"asset-12","recorder":{"recording":{"recorder_version":"axon_recorder 0.5.0"}}}`,
		},
	}

	bucket, objectName, ok := resolveDataOpsMP4Location("minio-bucket", row)
	if !ok {
		t.Fatal("resolveDataOpsMP4Location() ok = false, want true")
	}
	if bucket != "minio-bucket" {
		t.Fatalf("bucket = %q, want minio-bucket", bucket)
	}
	if objectName != "axon-transfer/capture.mcap" {
		t.Fatalf("objectName = %q, want axon-transfer/capture.mcap", objectName)
	}
}

func TestOpenBulkMP4ObjectKeepsMinIOAndTOSRoutesSeparate(t *testing.T) {
	minioClient, err := s3.Connect(&s3.Config{
		Endpoint:     "127.0.0.1:1",
		AccessKey:    "minio-ak",
		SecretKey:    "minio-sk",
		Bucket:       "minio-bucket",
		EnsureBucket: false,
	})
	if err != nil {
		t.Fatalf("create MinIO client: %v", err)
	}

	qa := NewEpisodeQAHandler(nil, minioClient, "minio-bucket", nil, &config.StorageConfig{
		Type:       "tos",
		Endpoint:   "tos-cn-beijing.volces.com",
		Bucket:     "tos-bucket",
		Region:     "cn-beijing",
		AccessKey:  "tos-ak",
		SecretKey:  "tos-sk",
		UseSSL:     true,
		STSRoleTRN: "trn:iam::123:role/qa-read",
	})
	qa.tos.stsClient = fakeEpisodeQASTSClient{}
	var tosRequests int
	qa.tos.client = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		tosRequests++
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader("tos-mcap")),
			ContentLength: int64(len("tos-mcap")),
		}, nil
	})}
	h := &DataOpsHandler{qa: qa}

	minioObject, err := h.openBulkMP4Object(context.Background(), "minio-bucket", "axon/capture.mcap")
	if err != nil {
		t.Fatalf("open MinIO object: %v", err)
	}
	if closeErr := minioObject.Close(); closeErr != nil {
		t.Fatalf("close MinIO object: %v", closeErr)
	}
	if tosRequests != 0 {
		t.Fatalf("opening MinIO object made %d TOS requests, want 0", tosRequests)
	}

	tosObject, err := h.openBulkMP4Object(context.Background(), "tos-bucket", "sdk/capture.mcap")
	if err != nil {
		t.Fatalf("open TOS object: %v", err)
	}
	tosBody, err := io.ReadAll(tosObject)
	if closeErr := tosObject.Close(); closeErr != nil {
		t.Fatalf("close TOS object: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("read TOS object: %v", err)
	}
	if string(tosBody) != "tos-mcap" {
		t.Fatalf("TOS body = %q, want tos-mcap", tosBody)
	}
	if tosRequests != 1 {
		t.Fatalf("TOS requests = %d, want 1", tosRequests)
	}
}

func TestBulkMP4ArchiveNameUsesEpisodeIDAndAvoidsDuplicates(t *testing.T) {
	used := map[string]struct{}{}
	first := uniqueBulkMP4ArchiveName(dataOpsBulkMP4EpisodeRow{ID: 1, EpisodeID: "ep/one"}, used)
	second := uniqueBulkMP4ArchiveName(dataOpsBulkMP4EpisodeRow{ID: 2, EpisodeID: "ep/one"}, used)
	third := uniqueBulkMP4ArchiveName(dataOpsBulkMP4EpisodeRow{ID: 3}, used)

	if first != "ep_one.mp4" {
		t.Fatalf("first archive name = %q, want ep_one.mp4", first)
	}
	if second != "ep_one_2.mp4" {
		t.Fatalf("second archive name = %q, want ep_one_2.mp4", second)
	}
	if third != "episode_3.mp4" {
		t.Fatalf("third archive name = %q, want episode_3.mp4", third)
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

func TestCancelBulkRunRequestsCancellationIdempotently(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	insertDataOpsBulkRunForTest(t, db, "bulk_qa_cancel", dataOpsBulkRunStatusRunning)

	for attempt := 1; attempt <= 2; attempt++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/bulk-runs/bulk_qa_cancel/cancel", nil)
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, body = %s", attempt, rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("attempt %d decode response: %v", attempt, err)
		}
		if got["run_id"] != "bulk_qa_cancel" || got["status"] != "cancel_requested" {
			t.Fatalf("attempt %d response = %+v, want cancel_requested run", attempt, got)
		}
		if got["cancel_requested_at"] == nil {
			t.Fatalf("attempt %d cancel_requested_at = nil", attempt)
		}
	}
}

func TestBulkRunTerminalUpdateFinalizesConcurrentCancellation(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := NewDataOpsHandler(db)
	insertDataOpsBulkRunForTest(t, db, "bulk_qa_cancel_race", dataOpsBulkRunStatusCancelRequested)
	if _, err := db.Exec(`
		UPDATE bulk_runs
		SET total_count = 5, processed_count = 2
		WHERE run_id = 'bulk_qa_cancel_race'
	`); err != nil {
		t.Fatalf("prepare cancel race run: %v", err)
	}

	run, err := h.markBulkRunTerminal(context.Background(), "bulk_qa_cancel_race", dataOpsBulkRunStatusCompleted, "")
	if err != nil {
		t.Fatalf("mark terminal after cancellation: %v", err)
	}
	if run.Status != dataOpsBulkRunStatusCanceled || run.ProcessedCount != 2 || run.CanceledCount != 3 {
		t.Fatalf("terminal cancellation run = %+v, want canceled with processed=2 canceled=3", run)
	}
}

func TestCancelBulkQARunStopsUndispatchedEpisodes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	started := make(chan int64, 6)
	release := make(chan struct{})
	h := &DataOpsHandler{db: db, qaRunner: cancelableDataOpsQARunner{started: started, release: release}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	for id := int64(1); id <= 6; id++ {
		insertDataOpsBulkTestEpisode(t, db, id, fmt.Sprintf("2026-06-%02dT00:00:00Z", id))
	}

	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-qa", bytes.NewBufferString(`{"confirm":true,"filters":{}}`))
	postReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", postRec.Code, postRec.Body.String())
	}
	var accepted struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode start response: %v", err)
	}

	for count := 0; count < dataOpsBulkQAConcurrency; count++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d QA episodes started", count)
		}
	}

	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/bulk-runs/"+accepted.Run.RunID+"/cancel", nil)
	router.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}
	close(release)

	got := waitForBulkRunStatus(t, router, accepted.Run.RunID, dataOpsBulkRunStatusCanceled)
	if got.TotalCount != 6 || got.ProcessedCount != 4 || got.CanceledCount != 2 || got.PassedCount != 4 {
		t.Fatalf("canceled run counts = %+v, want total=6 processed=4 canceled=2 passed=4", got)
	}
	select {
	case episodeID := <-started:
		t.Fatalf("episode %d started after cancellation", episodeID)
	default:
	}
}

func TestBulkSyncUsesCancelableBulkRun(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	if _, err := db.Exec(`INSERT INTO tasks (id, task_id, organization_id) VALUES (100, 'task-100', 12)`); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	for id := int64(1); id <= 2; id++ {
		if _, err := db.Exec(`
			INSERT INTO episodes (id, episode_id, task_id, qa_status, cloud_synced, deleted_at, created_at)
			VALUES (?, ?, 100, 'approved', 0, NULL, ?)
		`, id, fmt.Sprintf("episode-%d", id), fmt.Sprintf("2026-06-%02dT00:00:00Z", id)); err != nil {
			t.Fatalf("insert episode %d: %v", id, err)
		}
	}

	h := &DataOpsHandler{db: db, syncWorker: &fakeDataOpsBulkSyncWorker{db: db}}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	postRec := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/episodes/bulk-sync", bytes.NewBufferString(`{"confirm":true,"filters":{"workspace_id":"12"}}`))
	postReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, body = %s", postRec.Code, postRec.Body.String())
	}
	var accepted struct {
		Run DataOpsBulkRunResponse `json:"run"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if accepted.Run.Action != "bulk_sync" || accepted.Run.TotalCount != 2 {
		t.Fatalf("accepted run = %+v, want bulk_sync total=2", accepted.Run)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var pending int
		if err := db.Get(&pending, `SELECT COUNT(*) FROM sync_logs WHERE bulk_run_id = ? AND status = 'pending'`, accepted.Run.RunID); err != nil {
			t.Fatalf("count pending sync logs: %v", err)
		}
		if pending == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/bulk-runs/"+accepted.Run.RunID+"/cancel", nil)
	router.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}

	got := waitForBulkRunStatus(t, router, accepted.Run.RunID, dataOpsBulkRunStatusCanceled)
	if got.ProcessedCount != 0 || got.CanceledCount != 2 {
		t.Fatalf("canceled sync run = %+v, want processed=0 canceled=2", got)
	}
}

func TestWaitForBulkSyncPollDisablesCancellationWakeAfterRequest(t *testing.T) {
	ticker := make(chan time.Time, 1)
	ticker <- time.Now()
	cancelWake := make(chan struct{})
	close(cancelWake)

	remainingCancelWake := waitForBulkSyncPoll(ticker, cancelWake, true)
	if remainingCancelWake != nil {
		t.Fatal("cancel wake channel remains enabled after cancellation request")
	}
	select {
	case <-ticker:
		t.Fatal("poll returned through the closed cancellation channel instead of waiting for the ticker")
	default:
	}
}

func TestCancelBulkMP4RunKeepsCompletedPartialArchive(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDataOpsBulkPreviewTestDB(t)
	started := make(chan int64, 2)
	release := make(chan struct{})
	h := &DataOpsHandler{
		db: db,
		bulkMP4Converter: func(_ context.Context, row dataOpsBulkMP4EpisodeRow, _ string, mp4Dir string) (string, func(), error) {
			started <- row.ID
			<-release
			path := filepath.Join(mp4Dir, fmt.Sprintf("episode-%d.mp4", row.ID))
			if err := os.WriteFile(path, []byte("mp4"), 0o600); err != nil {
				return "", nil, err
			}
			return path, func() {}, nil
		},
	}
	router := gin.New()
	h.RegisterRoutes(router.Group("/api/v1/data-ops"))

	run, err := h.createBulkRun(context.Background(), dataOpsBulkRunActionMP4, 2)
	if err != nil {
		t.Fatalf("create MP4 run: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(h.bulkMP4ZipPath(run.RunID)); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove partial MP4 archive: %v", err)
		}
	})
	go h.runBulkEpisodeMP4(run.RunID, []dataOpsBulkMP4EpisodeRow{
		{ID: 1, EpisodeID: "episode-1"},
		{ID: 2, EpisodeID: "episode-2"},
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first MP4 conversion did not start")
	}
	cancelRec := httptest.NewRecorder()
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/v1/data-ops/bulk-runs/"+run.RunID+"/cancel", nil)
	router.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancelRec.Code, cancelRec.Body.String())
	}
	close(release)

	got := waitForBulkRunStatus(t, router, run.RunID, dataOpsBulkRunStatusCanceled)
	if got.ProcessedCount != 1 || got.CanceledCount != 1 || got.DownloadURL == "" {
		t.Fatalf("canceled MP4 run = %+v, want one generated, one canceled, and partial download", got)
	}
	if _, err := os.Stat(h.bulkMP4ZipPath(run.RunID)); err != nil {
		t.Fatalf("partial MP4 archive missing: %v", err)
	}
	select {
	case episodeID := <-started:
		t.Fatalf("episode %d started after cancellation", episodeID)
	default:
	}
}

func TestCancelQueuedBulkMP4RunCreatesDownloadableEmptyArchive(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := NewDataOpsHandler(db)
	run, err := h.createBulkRun(context.Background(), dataOpsBulkRunActionMP4, 2)
	if err != nil {
		t.Fatalf("create MP4 run: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(h.bulkMP4ZipPath(run.RunID)); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove empty MP4 archive: %v", err)
		}
	})
	if _, err := db.Exec(`UPDATE bulk_runs SET status = ? WHERE run_id = ?`, dataOpsBulkRunStatusCancelRequested, run.RunID); err != nil {
		t.Fatalf("request queued cancellation: %v", err)
	}

	h.runBulkEpisodeMP4(run.RunID, []dataOpsBulkMP4EpisodeRow{{ID: 1}, {ID: 2}})
	got, err := h.loadBulkRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("load canceled MP4 run: %v", err)
	}
	if got.Status != dataOpsBulkRunStatusCanceled || got.CanceledCount != 2 || got.DownloadURL == "" {
		t.Fatalf("queued canceled MP4 run = %+v, want canceled=2 with download", got)
	}
	if _, err := os.Stat(h.bulkMP4ZipPath(run.RunID)); err != nil {
		t.Fatalf("empty MP4 archive missing: %v", err)
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

func TestInterruptActiveBulkRunsMarksInFlightRunsInterrupted(t *testing.T) {
	db := setupDataOpsBulkPreviewTestDB(t)
	h := &DataOpsHandler{db: db}

	insertDataOpsBulkRunForTest(t, db, "bulk_qa_queued", dataOpsBulkRunStatusQueued)
	insertDataOpsBulkRunForTest(t, db, "bulk_qa_running", dataOpsBulkRunStatusRunning)
	insertDataOpsBulkRunForTest(t, db, "bulk_qa_completed", dataOpsBulkRunStatusCompleted)
	insertDataOpsBulkRunForTest(t, db, "bulk_sync_canceling", dataOpsBulkRunStatusCancelRequested)
	if _, err := db.Exec(`UPDATE bulk_runs SET action = 'bulk_sync' WHERE run_id = 'bulk_sync_canceling'`); err != nil {
		t.Fatalf("mark sync run action: %v", err)
	}

	if err := h.InterruptActiveBulkRuns(context.Background()); err != nil {
		t.Fatalf("InterruptActiveBulkRuns returned error: %v", err)
	}

	for _, runID := range []string{"bulk_qa_queued", "bulk_qa_running", "bulk_sync_canceling"} {
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
			workstation_id INTEGER,
			qa_status TEXT,
			cloud_synced BOOLEAN NOT NULL DEFAULT 0,
			deleted_at TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE tasks (
			id INTEGER PRIMARY KEY,
			task_id TEXT,
			dc_plan_id INTEGER,
			organization_id INTEGER,
			workstation_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE dc_plan (
			id INTEGER PRIMARY KEY,
			dc_project_id INTEGER,
			dc_project_name TEXT,
			dc_task_id INTEGER,
			dc_task_name TEXT,
			deleted_at TEXT
		)`,
		`CREATE TABLE workstations (
			id INTEGER PRIMARY KEY,
			robot_id INTEGER,
			data_collector_id INTEGER,
			workspace_id INTEGER,
			deleted_at TEXT
		)`,
		`CREATE TABLE robots (
			id INTEGER PRIMARY KEY,
			deleted_at TEXT
		)`,
		`CREATE TABLE data_collectors (id INTEGER PRIMARY KEY, deleted_at TEXT)`,
		`CREATE TABLE sync_logs (
			id INTEGER PRIMARY KEY,
			episode_id INTEGER NOT NULL,
			bulk_run_id TEXT,
			status TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_retry_at TIMESTAMP NULL,
			error_message TEXT,
			completed_at TIMESTAMP NULL
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
			canceled_count INTEGER NOT NULL DEFAULT 0,
			error_message TEXT,
			started_at TIMESTAMP NULL,
			cancel_requested_at TIMESTAMP NULL,
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
		INSERT INTO episodes (id, episode_id, task_id, qa_status, cloud_synced, deleted_at, created_at)
		VALUES (?, ?, 0, 'pending_qa', 0, NULL, ?)
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

type cancelableDataOpsQARunner struct {
	started chan<- int64
	release <-chan struct{}
}

type fakeDataOpsBulkSyncWorker struct {
	db *sqlx.DB
}

func (w *fakeDataOpsBulkSyncWorker) IsRunning() bool {
	return true
}

func (w *fakeDataOpsBulkSyncWorker) MaxRetries() int {
	return 3
}

func (w *fakeDataOpsBulkSyncWorker) EnqueueEpisodeManualForBulkRun(ctx context.Context, episodeID int64, bulkRunID string) error {
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO sync_logs (episode_id, bulk_run_id, status, attempt_count)
		VALUES (?, ?, 'pending', 0)
	`, episodeID, bulkRunID)
	return err
}

func (w *fakeDataOpsBulkSyncWorker) CancelBulkRun(ctx context.Context, bulkRunID string) (int64, error) {
	res, err := w.db.ExecContext(ctx, `
		UPDATE sync_logs SET status = 'canceled', completed_at = ?
		WHERE bulk_run_id = ? AND status = 'pending'
	`, time.Now().UTC(), bulkRunID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r cancelableDataOpsQARunner) RunEpisodeQASuite(_ context.Context, episodeID int64, mode QARunMode) (*EpisodeQASuiteResponse, error) {
	r.started <- episodeID
	<-r.release
	return &EpisodeQASuiteResponse{EpisodeID: episodeID, Passed: true, Mode: mode}, nil
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
