// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
)

const dataOpsBulkQAConcurrency = 4

// DataOpsBulkEpisodeFilters contains data-ops filters for bulk episode actions.
type DataOpsBulkEpisodeFilters struct {
	CreatedAtFrom       string `json:"created_at_from,omitempty"`
	CreatedAtTo         string `json:"created_at_to,omitempty"`
	Keyword             string `json:"q,omitempty"`
	QAStatus            string `json:"qa_status,omitempty"`
	SyncStatus          string `json:"sync_status,omitempty"`
	SceneID             string `json:"scene_id,omitempty"`
	SOPID               string `json:"sop_id,omitempty"`
	RobotTypeID         string `json:"robot_type_id,omitempty"`
	RobotDeviceID       string `json:"robot_device_id,omitempty"`
	CollectorOperatorID string `json:"collector_operator_id,omitempty"`
	Label               string `json:"label,omitempty"`
	Limit               string `json:"limit,omitempty"`
	Offset              string `json:"offset,omitempty"`
}

// DataOpsBulkEpisodeActionRequest is the request body for bulk preview and execute calls.
type DataOpsBulkEpisodeActionRequest struct {
	Confirm bool                      `json:"confirm,omitempty"`
	Filters DataOpsBulkEpisodeFilters `json:"filters,omitempty"`
}

// DataOpsBulkSkippedBreakdownItem summarizes one skipped reason in a bulk preview.
type DataOpsBulkSkippedBreakdownItem struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// DataOpsBulkEpisodePreviewResponse reports matched, eligible, and skipped counts before execution.
type DataOpsBulkEpisodePreviewResponse struct {
	Status               string                            `json:"status"`
	Action               string                            `json:"action"`
	MatchedCount         int                               `json:"matched_count"`
	EligibleCount        int                               `json:"eligible_count"`
	SkippedCount         int                               `json:"skipped_count"`
	ProtectedStatusCount int                               `json:"protected_status_count,omitempty"`
	SyncWorkerRunning    *bool                             `json:"sync_worker_running,omitempty"`
	SkippedBreakdown     []DataOpsBulkSkippedBreakdownItem `json:"skipped_breakdown"`
	Warnings             []string                          `json:"warnings"`
}

// DataOpsBulkEpisodeActionResponse acknowledges an accepted asynchronous bulk action.
type DataOpsBulkEpisodeActionResponse struct {
	Status       string `json:"status"`
	MatchedCount int    `json:"matched_count"`
	Message      string `json:"message"`
}

// DataOpsBulkEpisodeQAActionResponse acknowledges an accepted asynchronous bulk QA run.
type DataOpsBulkEpisodeQAActionResponse struct {
	Run     DataOpsBulkRunResponse `json:"run"`
	Message string                 `json:"message"`
}

type dataOpsBulkQAPreviewRow struct {
	MatchedCount         int64 `db:"matched_count"`
	QARunningCount       int64 `db:"qa_running_count"`
	ProtectedStatusCount int64 `db:"protected_status_count"`
}

type dataOpsBulkSyncPreviewRow struct {
	MatchedCount          int64 `db:"matched_count"`
	EligibleCount         int64 `db:"eligible_count"`
	QANotApprovedCount    int64 `db:"qa_not_approved_count"`
	AlreadySyncedCount    int64 `db:"already_synced_count"`
	SyncActiveCount       int64 `db:"sync_active_count"`
	UnsupportedSyncStatus int64 `db:"unsupported_sync_status_count"`
}

// PreviewBulkEpisodeQA previews a bulk QA run for the current data-ops filters.
//
// @Summary      Preview bulk episode QA
// @Description  Previews matched, eligible, and skipped episode counts for a filtered bulk QA operation.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  false  "Bulk preview filters"
// @Success      200      {object}  DataOpsBulkEpisodePreviewResponse
// @Failure      400      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-qa/preview [post]
func (h *DataOpsHandler) PreviewBulkEpisodeQA(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}

	preview, err := h.previewBulkEpisodeQA(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk QA preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk qa"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// PreviewBulkEpisodeSync previews a bulk cloud sync run for the current data-ops filters.
//
// @Summary      Preview bulk episode cloud sync
// @Description  Previews matched, eligible, and skipped episode counts for a filtered bulk cloud sync operation.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  false  "Bulk preview filters"
// @Success      200      {object}  DataOpsBulkEpisodePreviewResponse
// @Failure      400      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-sync/preview [post]
func (h *DataOpsHandler) PreviewBulkEpisodeSync(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}

	preview, err := h.previewBulkEpisodeSync(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk sync preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk sync"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// BulkRunEpisodeQA starts a filtered asynchronous bulk QA run.
//
// @Summary      Run bulk episode QA
// @Description  Accepts a filtered episode snapshot and starts an asynchronous bulk QA run.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  true  "Bulk QA filters and confirmation"
// @Success      202      {object}  DataOpsBulkEpisodeQAActionResponse
// @Failure      400      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-qa [post]
func (h *DataOpsHandler) BulkRunEpisodeQA(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	if !h.ensureBulkQAConfigured(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}

	h.bulkRunMu.Lock()
	defer h.bulkRunMu.Unlock()

	if current, exists, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionQA); err != nil {
		logger.Printf("[DATA_OPS] bulk QA current run lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current bulk run"})
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "bulk qa already running",
			"run_id": current.RunID,
			"status": current.Status,
		})
		return
	}

	ids, err := h.selectBulkEpisodeIDs(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk QA ID snapshot failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to select data operation episodes"})
		return
	}

	run, err := h.createBulkQARun(c.Request.Context(), int64(len(ids)))
	if err != nil {
		logger.Printf("[DATA_OPS] bulk QA run create failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bulk qa run"})
		return
	}

	logger.Printf("[DATA_OPS] Bulk QA accepted: run_id=%s total=%d", run.RunID, run.TotalCount)
	if len(ids) > 0 {
		go h.runBulkEpisodeQA(run.RunID, ids)
	}

	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeQAActionResponse{
		Run:     run,
		Message: fmt.Sprintf("%d episodes accepted for bulk QA", len(ids)),
	})
}

// BulkSyncEpisodes starts a filtered asynchronous bulk cloud sync run.
//
// @Summary      Run bulk episode cloud sync
// @Description  Accepts a filtered episode snapshot and starts asynchronous cloud sync enqueues.
// @Tags         data-ops
// @Accept       json
// @Produce      json
// @Param        request  body      DataOpsBulkEpisodeActionRequest  true  "Bulk sync filters and confirmation"
// @Success      202      {object}  DataOpsBulkEpisodeActionResponse
// @Failure      400      {object}  map[string]string
// @Failure      503      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Router       /data-ops/episodes/bulk-sync [post]
func (h *DataOpsHandler) BulkSyncEpisodes(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	if !h.ensureBulkSyncWorkerRunning(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}

	ids, err := h.selectBulkEpisodeIDs(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk sync ID snapshot failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to select data operation episodes"})
		return
	}

	logger.Printf("[DATA_OPS] Bulk sync accepted: matched=%d", len(ids))
	go h.runBulkEpisodeSync(ids)

	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeActionResponse{
		Status:       "accepted",
		MatchedCount: len(ids),
		Message:      fmt.Sprintf("%d episodes accepted for bulk cloud sync", len(ids)),
	})
}

func (h *DataOpsHandler) ensureDataOpsDatabase(c *gin.Context) bool {
	if h == nil || h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return false
	}
	return true
}

func (h *DataOpsHandler) ensureBulkQAConfigured(c *gin.Context) bool {
	if h.bulkQARunner() == nil || (h.qa != nil && (h.qa.db == nil || h.qa.s3 == nil)) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "qa service is not configured"})
		return false
	}
	return true
}

func (h *DataOpsHandler) ensureBulkSyncWorkerRunning(c *gin.Context) bool {
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sync worker is not configured"})
		return false
	}
	if !h.syncWorker.IsRunning() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": services.ErrSyncWorkerNotRunning.Error()})
		return false
	}
	return true
}

func (h *DataOpsHandler) parseBulkEpisodeActionRequest(c *gin.Context, requireConfirm bool) (DataOpsBulkEpisodeActionRequest, dataOpsEpisodeQuery, bool) {
	var req DataOpsBulkEpisodeActionRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bulk episode request"})
			return req, dataOpsEpisodeQuery{}, false
		}
	}

	if requireConfirm && !req.Confirm {
		c.JSON(http.StatusBadRequest, gin.H{"error": "confirm must be true"})
		return req, dataOpsEpisodeQuery{}, false
	}

	q, err := parseDataOpsBulkEpisodeFilters(req.Filters)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return req, dataOpsEpisodeQuery{}, false
	}
	return req, q, true
}

func parseDataOpsBulkEpisodeFilters(filters DataOpsBulkEpisodeFilters) (dataOpsEpisodeQuery, error) {
	qaStatuses, err := parseDataOpsBulkStringList(filters.QAStatus, "qa_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range qaStatuses {
		if _, ok := validDataProductionQAStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("qa_status must be one of pending_qa, qa_running, approved, needs_inspection, inspector_approved, rejected, failed")
		}
	}

	syncStatuses, err := parseDataOpsBulkStringList(filters.SyncStatus, "sync_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range syncStatuses {
		if _, ok := validDataOpsSyncStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("sync_status must be one of not_started, pending, in_progress, completed, failed")
		}
	}

	sceneIDs, err := parseDataOpsBulkPositiveInt64List(filters.SceneID, "scene_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	sopIDs, err := parseDataOpsBulkPositiveInt64List(filters.SOPID, "sop_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	robotTypeIDs, err := parseDataOpsBulkPositiveInt64List(filters.RobotTypeID, "robot_type_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	robotDeviceIDs, err := parseDataOpsBulkStringList(filters.RobotDeviceID, "robot_device_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	collectorOperatorIDs, err := parseDataOpsBulkStringList(filters.CollectorOperatorID, "collector_operator_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}

	out := dataOpsEpisodeQuery{
		Keyword:              strings.TrimSpace(filters.Keyword),
		QAStatuses:           qaStatuses,
		SyncStatuses:         syncStatuses,
		SceneIDs:             sceneIDs,
		SOPIDs:               sopIDs,
		RobotTypeIDs:         robotTypeIDs,
		RobotDeviceIDs:       robotDeviceIDs,
		CollectorOperatorIDs: collectorOperatorIDs,
		Label:                strings.TrimSpace(filters.Label),
	}

	if raw := strings.TrimSpace(filters.CreatedAtFrom); raw != "" {
		parsed, err := parseEpisodeRFC3339(raw)
		if err != nil {
			return dataOpsEpisodeQuery{}, fmt.Errorf("invalid created_at_from")
		}
		out.CreatedAtFrom = parsed
		out.HasCreatedAtFrom = true
	}
	if raw := strings.TrimSpace(filters.CreatedAtTo); raw != "" {
		parsed, err := parseEpisodeRFC3339(raw)
		if err != nil {
			return dataOpsEpisodeQuery{}, fmt.Errorf("invalid created_at_to")
		}
		out.CreatedAtTo = parsed
		out.HasCreatedAtTo = true
	}
	if out.HasCreatedAtFrom && out.HasCreatedAtTo && out.CreatedAtTo.Before(out.CreatedAtFrom) {
		return dataOpsEpisodeQuery{}, fmt.Errorf("created_at_to must be after created_at_from")
	}
	if len(out.Label) > maxMultiValueFilterStringItemLength {
		return dataOpsEpisodeQuery{}, fmt.Errorf("label contains a value longer than %d characters", maxMultiValueFilterStringItemLength)
	}

	return out, nil
}

func parseDataOpsBulkPositiveInt64List(raw string, fieldName string) ([]int64, error) {
	items, err := splitDataOpsBulkCommaItems(raw, fieldName, maxMultiValueFilterIntegerItemLength)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	seen := make(map[int64]struct{})
	values := []int64{}
	for _, item := range items {
		parsed, err := strconv.ParseInt(item, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid %s format", fieldName)
		}
		if _, ok := seen[parsed]; ok {
			continue
		}
		seen[parsed] = struct{}{}
		values = append(values, parsed)
	}
	return values, nil
}

func parseDataOpsBulkStringList(raw string, fieldName string) ([]string, error) {
	items, err := splitDataOpsBulkCommaItems(raw, fieldName, maxMultiValueFilterStringItemLength)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	values := []string{}
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	return values, nil
}

func splitDataOpsBulkCommaItems(raw string, fieldName string, maxItemLength int) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	items := []string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len(item) > maxItemLength {
			return nil, fmt.Errorf("%s contains a value longer than %d characters", fieldName, maxItemLength)
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *DataOpsHandler) previewBulkEpisodeQA(ctx context.Context, q dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	query := dataOpsBulkQAPreviewSQL(fromSQL, where)

	var row dataOpsBulkQAPreviewRow
	if err := h.db.GetContext(ctx, &row, query, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}

	matched := int(row.MatchedCount)
	qaRunning := int(row.QARunningCount)
	protected := int(row.ProtectedStatusCount)
	eligible := matched - qaRunning
	if eligible < 0 {
		eligible = 0
	}

	breakdown := []DataOpsBulkSkippedBreakdownItem{}
	if qaRunning > 0 {
		breakdown = append(breakdown, DataOpsBulkSkippedBreakdownItem{Reason: "qa_running", Count: qaRunning})
	}
	warnings := []string{}
	if protected > 0 {
		warnings = append(warnings, fmt.Sprintf("%d episodes are in protected manual QA statuses; checks can run but status will not be overwritten", protected))
	}

	return DataOpsBulkEpisodePreviewResponse{
		Status:               "preview",
		Action:               "bulk_qa",
		MatchedCount:         matched,
		EligibleCount:        eligible,
		SkippedCount:         qaRunning,
		ProtectedStatusCount: protected,
		SkippedBreakdown:     breakdown,
		Warnings:             warnings,
	}, nil
}

func dataOpsBulkQAPreviewSQL(fromSQL string, where string) string {
	return `
		SELECT
			COUNT(1) AS matched_count,
			COALESCE(SUM(CASE WHEN COALESCE(e.qa_status, '') = 'qa_running' THEN 1 ELSE 0 END), 0) AS qa_running_count,
			COALESCE(SUM(CASE WHEN COALESCE(e.qa_status, '') IN ('needs_inspection', 'inspector_approved', 'rejected') THEN 1 ELSE 0 END), 0) AS protected_status_count
	` + fromSQL + where
}

func (h *DataOpsHandler) previewBulkEpisodeSync(ctx context.Context, q dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL() + dataOpsLatestSyncPreviewJoinSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	query := dataOpsBulkSyncPreviewSQL(fromSQL, where)

	var row dataOpsBulkSyncPreviewRow
	if err := h.db.GetContext(ctx, &row, query, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}

	matched := int(row.MatchedCount)
	eligible := int(row.EligibleCount)
	skipped := matched - eligible
	if skipped < 0 {
		skipped = 0
	}
	workerRunning := h.syncWorker != nil && h.syncWorker.IsRunning()
	breakdown := []DataOpsBulkSkippedBreakdownItem{}
	appendBreakdown := func(reason string, count int64) {
		if count > 0 {
			breakdown = append(breakdown, DataOpsBulkSkippedBreakdownItem{Reason: reason, Count: int(count)})
		}
	}
	appendBreakdown("qa_not_approved", row.QANotApprovedCount)
	appendBreakdown("already_synced", row.AlreadySyncedCount)
	appendBreakdown("sync_active", row.SyncActiveCount)
	appendBreakdown("unsupported_sync_status", row.UnsupportedSyncStatus)

	warnings := []string{}
	if !workerRunning {
		warnings = append(warnings, "sync worker is not running; execution will be rejected until the worker starts")
	}

	return DataOpsBulkEpisodePreviewResponse{
		Status:            "preview",
		Action:            "bulk_sync",
		MatchedCount:      matched,
		EligibleCount:     eligible,
		SkippedCount:      skipped,
		SyncWorkerRunning: &workerRunning,
		SkippedBreakdown:  breakdown,
		Warnings:          warnings,
	}, nil
}

func dataOpsLatestSyncPreviewJoinSQL() string {
	return `
		LEFT JOIN (
			SELECT sl.episode_id, sl.status
			FROM sync_logs sl
			INNER JOIN (
				SELECT episode_id, MAX(id) AS latest_id
				FROM sync_logs
				GROUP BY episode_id
			) latest ON latest.episode_id = sl.episode_id AND latest.latest_id = sl.id
		) latest_sync ON latest_sync.episode_id = e.id
	`
}

func dataOpsBulkSyncPreviewSQL(fromSQL string, where string) string {
	approved := "(COALESCE(e.qa_status, '') IN ('approved', 'inspector_approved'))"
	latestStatus := "COALESCE(latest_sync.status, '')"
	synced := "(e.cloud_synced = TRUE OR " + latestStatus + " = 'completed')"
	active := "(" + latestStatus + " IN ('pending', 'in_progress'))"
	eligible := approved + " AND NOT " + synced + " AND (latest_sync.status IS NULL OR latest_sync.status = 'failed')"
	unsupported := approved + " AND NOT " + synced + " AND latest_sync.status IS NOT NULL AND latest_sync.status NOT IN ('pending', 'in_progress', 'completed', 'failed')"

	return `
		SELECT
			COUNT(1) AS matched_count,
			COALESCE(SUM(CASE WHEN ` + eligible + ` THEN 1 ELSE 0 END), 0) AS eligible_count,
			COALESCE(SUM(CASE WHEN NOT ` + approved + ` THEN 1 ELSE 0 END), 0) AS qa_not_approved_count,
			COALESCE(SUM(CASE WHEN ` + approved + ` AND ` + synced + ` THEN 1 ELSE 0 END), 0) AS already_synced_count,
			COALESCE(SUM(CASE WHEN ` + approved + ` AND NOT ` + synced + ` AND ` + active + ` THEN 1 ELSE 0 END), 0) AS sync_active_count,
			COALESCE(SUM(CASE WHEN ` + unsupported + ` THEN 1 ELSE 0 END), 0) AS unsupported_sync_status_count
	` + fromSQL + where
}

func (h *DataOpsHandler) selectBulkEpisodeIDs(ctx context.Context, q dataOpsEpisodeQuery) ([]int64, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	query := dataOpsEpisodeIDSnapshotSQL(fromSQL, where)

	ids := []int64{}
	if err := h.db.SelectContext(ctx, &ids, query, args...); err != nil {
		return nil, err
	}
	return ids, nil
}

func dataOpsEpisodeIDSnapshotSQL(fromSQL string, where string) string {
	return `
		SELECT e.id
	` + fromSQL + where + `
		ORDER BY e.created_at DESC, e.id DESC
	`
}

func (h *DataOpsHandler) runBulkEpisodeQA(runID string, ids []int64) {
	matched := int64(len(ids))
	if matched == 0 {
		logger.Printf("[DATA_OPS] Bulk QA completed: run_id=%s total=0 processed=0 passed=0 qa_failed=0 processing_failed=0 skipped=0", runID)
		return
	}
	runner := h.bulkQARunner()
	if runner == nil {
		logger.Printf("[DATA_OPS] Bulk QA failed: run_id=%s, err=qa runner is not configured", runID)
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, "qa runner is not configured")
		return
	}
	if _, err := h.markBulkRunRunning(context.Background(), runID); err != nil {
		logger.Printf("[DATA_OPS] Bulk QA failed to start: run_id=%s, err=%v", runID, err)
		return
	}

	workerCount := dataOpsBulkQAConcurrency
	if len(ids) < workerCount {
		workerCount = len(ids)
	}

	jobs := make(chan int64)
	results := make(chan dataOpsBulkQAEpisodeResult)
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for episodeID := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), defaultEpisodeQATimeout)
				result, err := runner.RunEpisodeQASuite(ctx, episodeID, qaRunModeManual)
				cancel()
				if err != nil {
					if isBulkQASkippedError(err) {
						results <- dataOpsBulkQAEpisodeResult{episodeID: episodeID, outcome: dataOpsBulkQAEpisodeSkipped}
						continue
					}
					logger.Printf("[DATA_OPS] Bulk QA failed: episode=%d, err=%v", episodeID, err)
					results <- dataOpsBulkQAEpisodeResult{episodeID: episodeID, outcome: dataOpsBulkQAEpisodeProcessingFailed}
					continue
				}
				if result == nil {
					logger.Printf("[DATA_OPS] Bulk QA failed: episode=%d, err=empty qa result", episodeID)
					results <- dataOpsBulkQAEpisodeResult{episodeID: episodeID, outcome: dataOpsBulkQAEpisodeProcessingFailed}
					continue
				}
				if result.Passed {
					results <- dataOpsBulkQAEpisodeResult{episodeID: episodeID, outcome: dataOpsBulkQAEpisodePassed}
				} else {
					results <- dataOpsBulkQAEpisodeResult{episodeID: episodeID, outcome: dataOpsBulkQAEpisodeFailed}
				}
			}
		}()
	}

	go func() {
		for _, episodeID := range ids {
			jobs <- episodeID
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var lastProgressPublishedAt time.Time
	for result := range results {
		run, err := h.incrementBulkQARunCounts(context.Background(), runID, result.outcome)
		if err != nil {
			logger.Printf("[DATA_OPS] Bulk QA progress update failed: run_id=%s episode=%d err=%v", runID, result.episodeID, err)
			continue
		}
		now := time.Now()
		if lastProgressPublishedAt.IsZero() || now.Sub(lastProgressPublishedAt) >= 500*time.Millisecond {
			h.publishBulkRunEvent("bulk_run_progress", run)
			lastProgressPublishedAt = now
		}
	}

	finalRun, err := h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusCompleted, "")
	if err != nil {
		logger.Printf("[DATA_OPS] Bulk QA completion update failed: run_id=%s err=%v", runID, err)
		return
	}

	logger.Printf(
		"[DATA_OPS] Bulk QA completed: run_id=%s total=%d processed=%d passed=%d qa_failed=%d processing_failed=%d skipped=%d",
		runID,
		matched,
		finalRun.ProcessedCount,
		finalRun.PassedCount,
		finalRun.QAFailedCount,
		finalRun.ProcessingFailedCount,
		finalRun.SkippedCount,
	)
}

func isBulkQASkippedError(err error) bool {
	return errors.Is(err, errEpisodeQAAlreadyRunning) ||
		errors.Is(err, errEpisodeQANotFound) ||
		errors.Is(err, errEpisodeQAAutoSkipped)
}

func (h *DataOpsHandler) runBulkEpisodeSync(ids []int64) {
	matched := int64(len(ids))
	var attempted int64
	var skipped int64
	var failed int64

	for _, episodeID := range ids {
		err := h.syncWorker.EnqueueEpisodeManual(context.Background(), episodeID)
		if err != nil {
			if isBulkSyncSkippedError(err) {
				skipped++
				continue
			}
			failed++
			logger.Printf("[DATA_OPS] Bulk sync enqueue failed: episode=%d, err=%v", episodeID, err)
			continue
		}
		attempted++
	}

	logger.Printf(
		"[DATA_OPS] Bulk sync completed: matched=%d, attempted=%d, skipped=%d, failed=%d",
		matched,
		attempted,
		skipped,
		failed,
	)
}

func isBulkSyncSkippedError(err error) bool {
	if errors.Is(err, services.ErrEpisodeAlreadyEnqueued) ||
		errors.Is(err, services.ErrSyncAlreadyInProgress) {
		return true
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already synced") ||
		strings.Contains(msg, "qa_status") ||
		strings.Contains(msg, "sync already completed")
}
