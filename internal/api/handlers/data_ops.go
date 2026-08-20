// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
)

const syncStatusNotStarted = "not_started"

var validDataOpsSyncStatuses = map[string]struct{}{
	syncStatusNotStarted: {},
	"pending":            {},
	"in_progress":        {},
	"completed":          {},
	"failed":             {},
	"canceled":           {},
}

// DataOpsHandler handles data operations APIs for the admin workbench.
type DataOpsHandler struct {
	db         *sqlx.DB
	qa         *EpisodeQAHandler
	qaRunner   dataOpsEpisodeQARunner
	syncWorker dataOpsBulkSyncWorker
	bulkRunMu  sync.Mutex
	// bulkRunBrokerMu protects lazy initialization of bulkRunBroker.
	bulkRunBrokerMu sync.Mutex
	bulkRunBroker   *dataOpsBulkRunBroker
	// bulkRunCancelMu protects the in-memory execution state for active runs.
	bulkRunCancelMu   sync.Mutex
	bulkRunExecutions map[string]*dataOpsBulkRunExecution
	bulkMP4Converter  func(context.Context, dataOpsBulkMP4EpisodeRow, string, string) (string, func(), error)
	stereoSplit       dataOpsStereoSplitManager
	depthNorm         dataOpsDepthNormalizer
	stereoBulkMu      sync.Mutex
	stereoBulkRuns    map[string]struct{}
}

// SetStereoSplitManager wires the durable stereo-split module.
func (h *DataOpsHandler) SetStereoSplitManager(manager dataOpsStereoSplitManager) {
	if h != nil {
		h.stereoSplit = manager
	}
}

type dataOpsBulkRunExecution struct {
	mu              sync.Mutex
	cancel          context.CancelFunc
	cancelRequested bool
}

type dataOpsBulkSyncWorker interface {
	IsRunning() bool
	MaxRetries() int
	EnqueueEpisodeManualForBulkRun(ctx context.Context, episodeID int64, bulkRunID string) error
	EnqueueStereoSplitManual(ctx context.Context, episodeID int64) error
	CancelBulkRun(ctx context.Context, bulkRunID string) (int64, error)
}

// NewDataOpsHandler creates a data operations handler.
func NewDataOpsHandler(db *sqlx.DB) *DataOpsHandler {
	return &DataOpsHandler{
		db:                db,
		bulkRunBroker:     newDataOpsBulkRunBroker(),
		bulkRunExecutions: make(map[string]*dataOpsBulkRunExecution),
		stereoBulkRuns:    make(map[string]struct{}),
	}
}

// SetBulkActionDeps wires optional services used by data-ops bulk actions.
func (h *DataOpsHandler) SetBulkActionDeps(qa *EpisodeQAHandler, syncWorker *services.SyncWorker) {
	if h == nil {
		return
	}
	h.qa = qa
	h.qaRunner = qa
	h.syncWorker = nil
	if syncWorker != nil {
		h.syncWorker = syncWorker
	}
}

// RegisterRoutes registers data operations routes under /data-ops.
func (h *DataOpsHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/episodes", h.ListEpisodes)
	apiV1.POST("/episodes/bulk-qa/preview", h.PreviewBulkEpisodeQA)
	apiV1.POST("/episodes/bulk-sync/preview", h.PreviewBulkEpisodeSync)
	apiV1.POST("/episodes/bulk-mp4/preview", h.PreviewBulkEpisodeMP4)
	if h.depthNorm != nil {
		apiV1.POST("/episodes/bulk-depth-normalization/preview", h.PreviewBulkDepthNormalization)
	}
	apiV1.POST("/episodes/bulk-qa", h.BulkRunEpisodeQA)
	apiV1.POST("/episodes/bulk-sync", h.BulkSyncEpisodes)
	apiV1.POST("/episodes/bulk-mp4", h.BulkExportEpisodeMP4)
	if h.depthNorm != nil {
		apiV1.POST("/episodes/bulk-depth-normalization", h.BulkDepthNormalization)
	}
	apiV1.GET("/episodes/:id/mp4", h.DownloadEpisodeMP4)
	apiV1.GET("/bulk-runs/current", h.GetCurrentBulkRun)
	apiV1.GET("/bulk-runs/:run_id", h.GetBulkRun)
	apiV1.GET("/bulk-runs/:run_id/items", h.ListBulkRunItems)
	apiV1.POST("/bulk-runs/:run_id/cancel", h.CancelBulkRun)
	apiV1.GET("/bulk-runs/:run_id/download", h.DownloadBulkMP4)
	apiV1.GET("/bulk-runs/:run_id/stream", h.StreamBulkRun)
	apiV1.GET("/episodes/:id/derivatives/depth-normalization", h.GetDepthNormalization)
	if h.depthNorm != nil {
		apiV1.POST("/episodes/:id/derivatives/depth-normalization", h.StartDepthNormalization)
	}
	h.registerStereoSplitSettingsRoutes(apiV1)
	if h.stereoSplit != nil {
		h.registerStereoSplitRoutes(apiV1)
	}
}

type dataOpsEpisodeQuery struct {
	Pagination            PaginationParams
	HasExplicitEpisodeIDs bool
	IncludedEpisodeIDs    []int64
	ExcludedEpisodeIDs    []int64
	WorkspaceIDs          []int64
	CreatedAtFrom         time.Time
	CreatedAtTo           time.Time
	HasCreatedAtFrom      bool
	HasCreatedAtTo        bool
	Keyword               string
	QAStatuses            []string
	SyncStatuses          []string
	RobotDeviceIDs        []string
	DeviceTypes           []string
	CollectorOperatorIDs  []string
	DCProjectIDs          []int64
	DCTaskIDs             []int64
	DCProjectName         string
	DCTaskName            string
	Label                 string
}

type dataOpsEpisodeRow struct {
	ID                  int64           `db:"id"`
	EpisodeID           string          `db:"episode_id"`
	TaskID              int64           `db:"task_id"`
	TaskPublicID        sql.NullString  `db:"task_public_id"`
	DCProjectID         sql.NullInt64   `db:"dc_project_id"`
	DCProjectName       sql.NullString  `db:"dc_project_name"`
	DCTaskID            sql.NullInt64   `db:"dc_task_id"`
	DCTaskName          sql.NullString  `db:"dc_task_name"`
	RobotDeviceID       sql.NullString  `db:"robot_device_id"`
	RobotMetadata       sql.NullString  `db:"robot_metadata"`
	CollectorOperatorID sql.NullString  `db:"collector_operator_id"`
	CollectorName       sql.NullString  `db:"collector_name"`
	QAStatus            string          `db:"qa_status"`
	QualityFlag         sql.NullString  `db:"quality_flag"`
	CloudSynced         bool            `db:"cloud_synced"`
	DurationSec         sql.NullFloat64 `db:"duration_sec"`
	FileSizeBytes       sql.NullInt64   `db:"file_size_bytes"`
	LabelsJSON          sql.NullString  `db:"labels"`
	CreatedAt           time.Time       `db:"created_at"`
}

// DataOpsEpisodeItemResponse describes one episode row in the data operations table.
type DataOpsEpisodeItemResponse struct {
	ID                  int64                         `json:"id"`
	EpisodeID           string                        `json:"episode_id"`
	TaskID              int64                         `json:"task_id"`
	TaskPublicID        *string                       `json:"task_public_id,omitempty"`
	DCProjectID         *int64                        `json:"dc_project_id,omitempty"`
	DCProjectName       *string                       `json:"dc_project_name,omitempty"`
	DCTaskID            *int64                        `json:"dc_task_id,omitempty"`
	DCTaskName          *string                       `json:"dc_task_name,omitempty"`
	RobotDeviceID       *string                       `json:"robot_device_id,omitempty"`
	RobotDeviceName     *string                       `json:"robot_device_name,omitempty"`
	CollectorOperatorID *string                       `json:"collector_operator_id,omitempty"`
	CollectorName       *string                       `json:"collector_name,omitempty"`
	QAStatus            string                        `json:"qa_status"`
	QualityFlag         *string                       `json:"quality_flag,omitempty"`
	LatestQACheck       *EpisodeQACheckRecordResponse `json:"latest_qa_check,omitempty"`
	SyncStatus          string                        `json:"sync_status"`
	LatestSyncLog       *SyncJobResponse              `json:"latest_sync_log,omitempty"`
	CloudSynced         bool                          `json:"cloud_synced"`
	DurationSec         *float64                      `json:"duration_sec,omitempty"`
	FileSizeBytes       *int64                        `json:"file_size_bytes,omitempty"`
	Labels              []string                      `json:"labels"`
	CreatedAt           string                        `json:"created_at"`
}

// DataOpsEpisodeListResponse contains paginated episode rows for data operations.
type DataOpsEpisodeListResponse struct {
	Items   []DataOpsEpisodeItemResponse `json:"items"`
	Total   int                          `json:"total"`
	Limit   int                          `json:"limit"`
	Offset  int                          `json:"offset"`
	HasNext bool                         `json:"hasNext,omitempty"`
	HasPrev bool                         `json:"hasPrev,omitempty"`
}

// ListEpisodes returns unified episode detail rows for data operations.
//
// @Summary      List data operation episodes
// @Description  Lists episode details with latest QA and cloud sync states.
// @Tags         data-ops
// @Produce      json
// @Param        limit                  query     int     false  "Max results"
// @Param        offset                 query     int     false  "Pagination offset"
// @Param        workspace_id           query     string  false  "Comma-separated Workspace IDs"
// @Param        created_at_from        query     string  false  "created_at >= RFC3339"
// @Param        created_at_to          query     string  false  "created_at <= RFC3339"
// @Param        q                      query     string  false  "Search episode/task/quality text"
// @Param        qa_status              query     string  false  "Comma-separated QA statuses"
// @Param        sync_status            query     string  false  "Comma-separated sync statuses: not_started,pending,in_progress,completed,failed,canceled"
// @Param        robot_device_id        query     string  false  "Comma-separated robot device IDs"
// @Param        device_type            query     string  false  "Comma-separated robot device types"
// @Param        collector_operator_id  query     string  false  "Comma-separated collector operator IDs"
// @Param        dc_project_id          query     string  false  "Comma-separated Hilbert project IDs"
// @Param        dc_project_name        query     string  false  "Fuzzy Hilbert project name filter"
// @Param        dc_task_id             query     string  false  "Comma-separated Hilbert task IDs"
// @Param        dc_task_name           query     string  false  "Fuzzy Hilbert task name filter"
// @Param        label                  query     string  false  "Exact label"
// @Success      200                    {object}  DataOpsEpisodeListResponse
// @Failure      400                    {object}  map[string]string
// @Failure      500                    {object}  map[string]string
// @Router       /data-ops/episodes [get]
func (h *DataOpsHandler) ListEpisodes(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return
	}

	q, err := parseDataOpsEpisodeQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	countQuery := "SELECT COUNT(1) " + fromSQL + where

	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[DATA_OPS] episode count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count data operation episodes"})
		return
	}

	query := dataOpsEpisodeListSQL(fromSQL, where)
	queryArgs := append(append([]interface{}{}, args...), q.Pagination.Limit, q.Pagination.Offset)

	var rows []dataOpsEpisodeRow
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[DATA_OPS] episode list query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data operation episodes"})
		return
	}

	episodeIDs := dataOpsEpisodeIDs(rows)
	latestQAChecks, err := h.latestQAChecksByEpisode(c.Request.Context(), episodeIDs)
	if err != nil {
		logger.Printf("[DATA_OPS] latest QA query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data operation episodes"})
		return
	}
	latestSyncLogs, err := h.latestSyncLogsByEpisode(c.Request.Context(), episodeIDs)
	if err != nil {
		logger.Printf("[DATA_OPS] latest sync query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data operation episodes"})
		return
	}

	items := make([]DataOpsEpisodeItemResponse, 0, len(rows))
	for _, row := range rows {
		item := dataOpsEpisodeItemFromRow(row)
		if qaCheck, ok := latestQAChecks[row.ID]; ok {
			item.LatestQACheck = qaCheck
		}
		if syncLog, ok := latestSyncLogs[row.ID]; ok {
			log := syncLog
			item.LatestSyncLog = &log
			item.SyncStatus = log.Status
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, DataOpsEpisodeListResponse{
		Items:   items,
		Total:   total,
		Limit:   q.Pagination.Limit,
		Offset:  q.Pagination.Offset,
		HasNext: q.Pagination.Offset+q.Pagination.Limit < total,
		HasPrev: q.Pagination.Offset > 0,
	})
}

func parseDataOpsEpisodeQuery(c *gin.Context) (dataOpsEpisodeQuery, error) {
	pagination, err := ParsePagination(c)
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	workspaceIDs, err := parseNonNegativeInt64List(c.Query("workspace_id"), "workspace_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}

	qaStatuses, err := parseStatsStringListQuery(c, "qa_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range qaStatuses {
		if _, ok := validDataProductionQAStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("qa_status must be one of pending_qa, qa_running, approved, failed, manual_review_failed")
		}
	}

	syncStatuses, err := parseStatsStringListQuery(c, "sync_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range syncStatuses {
		if _, ok := validDataOpsSyncStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("sync_status must be one of not_started, pending, in_progress, completed, failed, canceled")
		}
	}

	robotDeviceIDs, err := parseStatsStringListQuery(c, "robot_device_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	deviceTypes, err := parseStatsStringListQuery(c, "device_type")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	collectorOperatorIDs, err := parseStatsStringListQuery(c, "collector_operator_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	dcProjectIDs, err := parseNonNegativeInt64List(c.Query("dc_project_id"), "dc_project_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	dcTaskIDs, err := parseNonNegativeInt64List(c.Query("dc_task_id"), "dc_task_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}

	out := dataOpsEpisodeQuery{
		Pagination:           pagination,
		WorkspaceIDs:         workspaceIDs,
		Keyword:              strings.TrimSpace(c.Query("q")),
		QAStatuses:           qaStatuses,
		SyncStatuses:         syncStatuses,
		RobotDeviceIDs:       robotDeviceIDs,
		DeviceTypes:          deviceTypes,
		CollectorOperatorIDs: collectorOperatorIDs,
		DCProjectIDs:         dcProjectIDs,
		DCTaskIDs:            dcTaskIDs,
		DCProjectName:        strings.TrimSpace(c.Query("dc_project_name")),
		DCTaskName:           strings.TrimSpace(c.Query("dc_task_name")),
		Label:                strings.TrimSpace(c.Query("label")),
	}

	if raw := strings.TrimSpace(c.Query("created_at_from")); raw != "" {
		parsed, err := parseEpisodeRFC3339(raw)
		if err != nil {
			return dataOpsEpisodeQuery{}, fmt.Errorf("invalid created_at_from")
		}
		out.CreatedAtFrom = parsed
		out.HasCreatedAtFrom = true
	}
	if raw := strings.TrimSpace(c.Query("created_at_to")); raw != "" {
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
	if len(out.DCProjectName) > maxMultiValueFilterStringItemLength {
		return dataOpsEpisodeQuery{}, fmt.Errorf("dc_project_name contains a value longer than %d characters", maxMultiValueFilterStringItemLength)
	}
	if len(out.DCTaskName) > maxMultiValueFilterStringItemLength {
		return dataOpsEpisodeQuery{}, fmt.Errorf("dc_task_name contains a value longer than %d characters", maxMultiValueFilterStringItemLength)
	}

	return out, nil
}

func dataOpsEpisodeBaseFromSQL() string {
	return `
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
	`
}

func buildDataOpsEpisodeWhere(q dataOpsEpisodeQuery) (string, []interface{}) {
	where := " WHERE e.deleted_at IS NULL"
	args := []interface{}{}
	if q.HasExplicitEpisodeIDs {
		if len(q.IncludedEpisodeIDs) == 0 {
			where += " AND 1 = 0"
		} else {
			where, args = appendInt64InFilter(where, args, "e.id", q.IncludedEpisodeIDs)
		}
	} else {
		where, args = appendInt64NotInFilter(where, args, "e.id", q.ExcludedEpisodeIDs)
	}

	if q.HasCreatedAtFrom {
		where += " AND e.created_at >= ?"
		args = append(args, q.CreatedAtFrom)
	}
	if q.HasCreatedAtTo {
		where += " AND e.created_at <= ?"
		args = append(args, q.CreatedAtTo)
	}
	where, args = appendInt64InFilter(where, args, "COALESCE(t.organization_id, ws.workspace_id)", q.WorkspaceIDs)

	where, args = appendStringInFilter(where, args, "e.qa_status", q.QAStatuses)
	where, args = appendStringInFilter(where, args, "COALESCE(NULLIF(r.device_id, ''), NULLIF(ws.robot_serial, ''), '')", q.RobotDeviceIDs)
	where, args = appendStringInFilter(where, args, "COALESCE(r.device_type, '')", q.DeviceTypes)
	where, args = appendStringInFilter(where, args, "COALESCE(NULLIF(dc.operator_id, ''), NULLIF(ws.collector_operator_id, ''), '')", q.CollectorOperatorIDs)
	where, args = appendInt64InFilter(where, args, "dp.dc_project_id", q.DCProjectIDs)
	where, args = appendInt64InFilter(where, args, "dp.dc_task_id", q.DCTaskIDs)

	if q.Keyword != "" {
		where, args = appendKeywordSearch(where, args, q.Keyword, "e.episode_id", "t.task_id", "dp.dc_project_name", "dp.dc_task_name", "e.quality_flag")
	}
	if q.DCProjectName != "" {
		where, args = appendKeywordSearch(where, args, q.DCProjectName, "dp.dc_project_name")
	}
	if q.DCTaskName != "" {
		where, args = appendKeywordSearch(where, args, q.DCTaskName, "dp.dc_task_name")
	}
	if q.Label != "" {
		where += " AND JSON_CONTAINS(COALESCE(e.labels, JSON_ARRAY()), JSON_QUOTE(?))"
		args = append(args, q.Label)
	}
	if len(q.SyncStatuses) > 0 {
		syncWhere, syncArgs := dataOpsSyncStatusWhere(q.SyncStatuses)
		where += syncWhere
		args = append(args, syncArgs...)
	}

	return where, args
}

func appendInt64NotInFilter(whereClause string, args []interface{}, column string, values []int64) (string, []interface{}) {
	if len(values) == 0 {
		return whereClause, args
	}

	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return whereClause + " AND " + column + " NOT IN (" + strings.Join(placeholders, ",") + ")", args
}

func dataOpsSyncStatusWhere(statuses []string) (string, []interface{}) {
	if len(statuses) == 0 {
		return "", nil
	}

	hasNotStarted := false
	latestStatuses := []string{}
	for _, status := range statuses {
		if status == syncStatusNotStarted {
			hasNotStarted = true
			continue
		}
		latestStatuses = append(latestStatuses, status)
	}

	parts := []string{}
	args := []interface{}{}
	if hasNotStarted {
		parts = append(parts, "NOT EXISTS (SELECT 1 FROM sync_logs sl0 WHERE sl0.episode_id = e.id)")
	}
	if len(latestStatuses) > 0 {
		placeholders := make([]string, 0, len(latestStatuses))
		for _, status := range latestStatuses {
			placeholders = append(placeholders, "?")
			args = append(args, status)
		}
		parts = append(parts, `
			EXISTS (
				SELECT 1
				FROM sync_logs sl_latest
				WHERE sl_latest.episode_id = e.id
				  AND sl_latest.id = (
					SELECT MAX(sl2.id)
					FROM sync_logs sl2
					WHERE sl2.episode_id = e.id
				  )
				  AND sl_latest.status IN (`+strings.Join(placeholders, ",")+`)
			)
		`)
	}

	return " AND (" + strings.Join(parts, " OR ") + ")", args
}

func dataOpsEpisodeListSQL(fromSQL string, where string) string {
	return `
		SELECT
			e.id,
			e.episode_id,
			e.task_id,
			t.task_id AS task_public_id,
			dp.dc_project_id,
			dp.dc_project_name,
			dp.dc_task_id,
			dp.dc_task_name,
			COALESCE(NULLIF(r.device_id, ''), NULLIF(ws.robot_serial, '')) AS robot_device_id,
			r.metadata AS robot_metadata,
			COALESCE(NULLIF(dc.operator_id, ''), NULLIF(ws.collector_operator_id, '')) AS collector_operator_id,
			COALESCE(NULLIF(dc.name, ''), NULLIF(ws.collector_name, '')) AS collector_name,
			COALESCE(e.qa_status, '') AS qa_status,
			e.quality_flag,
			e.cloud_synced,
			e.duration_sec,
			e.file_size_bytes,
			e.labels,
			e.created_at
	` + fromSQL + where + `
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT ? OFFSET ?
	`
}

func dataOpsEpisodeIDs(rows []dataOpsEpisodeRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func (h *DataOpsHandler) latestQAChecksByEpisode(ctx context.Context, episodeIDs []int64) (map[int64]*EpisodeQACheckRecordResponse, error) {
	out := make(map[int64]*EpisodeQACheckRecordResponse)
	if len(episodeIDs) == 0 {
		return out, nil
	}

	query, args := dataOpsLatestQAChecksSQL(episodeIDs)
	var rows []episodeQACheckDBRow
	if err := h.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		record := qaCheckRecordFromDBRow(row)
		out[row.EpisodeID] = &record
	}
	return out, nil
}

func dataOpsLatestQAChecksSQL(episodeIDs []int64) (string, []interface{}) {
	placeholders, args := int64Placeholders(episodeIDs)
	return `
		SELECT qc.id, qc.episode_id, qc.check_name, qc.passed, qc.score, qc.details, qc.check_metadata, qc.checked_at
		FROM qa_checks qc
		INNER JOIN (
			SELECT episode_id, MAX(id) AS latest_id
			FROM qa_checks
			WHERE episode_id IN (` + placeholders + `)
			GROUP BY episode_id
		) latest ON latest.episode_id = qc.episode_id AND latest.latest_id = qc.id
	`, args
}

func (h *DataOpsHandler) latestSyncLogsByEpisode(ctx context.Context, episodeIDs []int64) (map[int64]SyncJobResponse, error) {
	out := make(map[int64]SyncJobResponse)
	if len(episodeIDs) == 0 {
		return out, nil
	}

	query, args := dataOpsLatestSyncLogsSQL(episodeIDs)
	var rows []syncLogRow
	if err := h.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.EpisodeID] = syncJobResponseFromRow(row)
	}
	return out, nil
}

func dataOpsLatestSyncLogsSQL(episodeIDs []int64) (string, []interface{}) {
	placeholders, args := int64Placeholders(episodeIDs)
	return `
		SELECT
			sl.id,
			sl.episode_id,
			e.episode_id AS episode_public_id,
				sl.source_path,
			sl.destination_path,
			sl.status,
			sl.bytes_transferred,
			sl.duration_sec,
			sl.error_message,
			COALESCE(sl.attempt_count, 0) AS attempt_count,
			sl.next_retry_at,
			sl.started_at,
			sl.completed_at
		FROM sync_logs sl
		INNER JOIN (
			SELECT episode_id, MAX(id) AS latest_id
			FROM sync_logs
			WHERE episode_id IN (` + placeholders + `)
			GROUP BY episode_id
		) latest ON latest.episode_id = sl.episode_id AND latest.latest_id = sl.id
		LEFT JOIN episodes e ON e.id = sl.episode_id AND e.deleted_at IS NULL
	`, args
}

func int64Placeholders(values []int64) (string, []interface{}) {
	placeholders := make([]string, 0, len(values))
	args := make([]interface{}, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return strings.Join(placeholders, ","), args
}

func dataOpsEpisodeItemFromRow(row dataOpsEpisodeRow) DataOpsEpisodeItemResponse {
	robotDeviceName := robotDeviceNameFromMetadata(row.RobotMetadata)
	return DataOpsEpisodeItemResponse{
		ID:                  row.ID,
		EpisodeID:           row.EpisodeID,
		TaskID:              row.TaskID,
		TaskPublicID:        nullableString(row.TaskPublicID),
		DCProjectID:         nullableInt64(row.DCProjectID),
		DCProjectName:       nullableString(row.DCProjectName),
		DCTaskID:            nullableInt64(row.DCTaskID),
		DCTaskName:          nullableString(row.DCTaskName),
		RobotDeviceID:       nullableString(row.RobotDeviceID),
		RobotDeviceName:     nullableString(sql.NullString{String: robotDeviceName, Valid: robotDeviceName != ""}),
		CollectorOperatorID: nullableString(row.CollectorOperatorID),
		CollectorName:       nullableString(row.CollectorName),
		QAStatus:            row.QAStatus,
		QualityFlag:         nullableString(row.QualityFlag),
		SyncStatus:          syncStatusNotStarted,
		CloudSynced:         row.CloudSynced,
		DurationSec:         nullableFloat64(row.DurationSec),
		FileSizeBytes:       nullableInt64(row.FileSizeBytes),
		Labels:              episodeLabelsFromDB(row.LabelsJSON),
		CreatedAt:           row.CreatedAt.UTC().Format(time.RFC3339),
	}
}
