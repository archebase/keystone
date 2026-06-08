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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
)

const syncStatusNotStarted = "not_started"

var validDataOpsSyncStatuses = map[string]struct{}{
	syncStatusNotStarted: {},
	"pending":            {},
	"in_progress":        {},
	"completed":          {},
	"failed":             {},
}

// DataOpsHandler handles data operations APIs for the admin workbench.
type DataOpsHandler struct {
	db *sqlx.DB
}

// NewDataOpsHandler creates a data operations handler.
func NewDataOpsHandler(db *sqlx.DB) *DataOpsHandler {
	return &DataOpsHandler{db: db}
}

// RegisterRoutes registers data operations routes under /data-ops.
func (h *DataOpsHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/episodes", h.ListEpisodes)
}

type dataOpsEpisodeQuery struct {
	Pagination           PaginationParams
	CreatedAtFrom        time.Time
	CreatedAtTo          time.Time
	HasCreatedAtFrom     bool
	HasCreatedAtTo       bool
	Keyword              string
	QAStatuses           []string
	SyncStatuses         []string
	SceneIDs             []int64
	SOPIDs               []int64
	RobotTypeIDs         []int64
	RobotDeviceIDs       []string
	CollectorOperatorIDs []string
	Label                string
}

type dataOpsEpisodeRow struct {
	ID                  int64           `db:"id"`
	EpisodeID           string          `db:"episode_id"`
	TaskID              int64           `db:"task_id"`
	TaskPublicID        sql.NullString  `db:"task_public_id"`
	SOPID               sql.NullInt64   `db:"sop_id"`
	SOP                 sql.NullString  `db:"sop"`
	SceneID             int64           `db:"scene_id"`
	SceneName           sql.NullString  `db:"scene_name"`
	RobotTypeID         sql.NullInt64   `db:"robot_type_id"`
	RobotType           sql.NullString  `db:"robot_type"`
	RobotDeviceID       sql.NullString  `db:"robot_device_id"`
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

type DataOpsEpisodeItemResponse struct {
	ID                  int64                         `json:"id"`
	EpisodeID           string                        `json:"episode_id"`
	TaskID              int64                         `json:"task_id"`
	TaskPublicID        *string                       `json:"task_public_id,omitempty"`
	SOPID               *int64                        `json:"sop_id,omitempty"`
	SOP                 *string                       `json:"sop,omitempty"`
	SceneID             int64                         `json:"scene_id"`
	SceneName           *string                       `json:"scene_name,omitempty"`
	RobotTypeID         *int64                        `json:"robot_type_id,omitempty"`
	RobotType           *string                       `json:"robot_type,omitempty"`
	RobotDeviceID       *string                       `json:"robot_device_id,omitempty"`
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
// @Param        created_at_from        query     string  false  "created_at >= RFC3339"
// @Param        created_at_to          query     string  false  "created_at <= RFC3339"
// @Param        q                      query     string  false  "Search episode/task/quality text"
// @Param        qa_status              query     string  false  "Comma-separated QA statuses"
// @Param        sync_status            query     string  false  "Comma-separated sync statuses: not_started,pending,in_progress,completed,failed"
// @Param        scene_id               query     string  false  "Comma-separated scene IDs"
// @Param        sop_id                 query     string  false  "Comma-separated SOP IDs"
// @Param        robot_type_id          query     string  false  "Comma-separated robot type IDs"
// @Param        robot_device_id        query     string  false  "Comma-separated robot device IDs"
// @Param        collector_operator_id  query     string  false  "Comma-separated collector operator IDs"
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

	qaStatuses, err := parseStatsStringListQuery(c, "qa_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range qaStatuses {
		if _, ok := validDataProductionQAStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("qa_status must be one of pending_qa, qa_running, approved, needs_inspection, inspector_approved, rejected, failed")
		}
	}

	syncStatuses, err := parseStatsStringListQuery(c, "sync_status")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	for _, status := range syncStatuses {
		if _, ok := validDataOpsSyncStatuses[status]; !ok {
			return dataOpsEpisodeQuery{}, fmt.Errorf("sync_status must be one of not_started, pending, in_progress, completed, failed")
		}
	}

	sceneIDs, err := parsePositiveInt64List(c.Query("scene_id"), "scene_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	sopIDs, err := parsePositiveInt64List(c.Query("sop_id"), "sop_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	robotTypeIDs, err := parsePositiveInt64List(c.Query("robot_type_id"), "robot_type_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	robotDeviceIDs, err := parseStatsStringListQuery(c, "robot_device_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}
	collectorOperatorIDs, err := parseStatsStringListQuery(c, "collector_operator_id")
	if err != nil {
		return dataOpsEpisodeQuery{}, err
	}

	out := dataOpsEpisodeQuery{
		Pagination:           pagination,
		Keyword:              strings.TrimSpace(c.Query("q")),
		QAStatuses:           qaStatuses,
		SyncStatuses:         syncStatuses,
		SceneIDs:             sceneIDs,
		SOPIDs:               sopIDs,
		RobotTypeIDs:         robotTypeIDs,
		RobotDeviceIDs:       robotDeviceIDs,
		CollectorOperatorIDs: collectorOperatorIDs,
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

	return out, nil
}

func dataOpsEpisodeBaseFromSQL() string {
	return `
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN scenes sc ON sc.id = e.scene_id AND sc.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		LEFT JOIN sops s ON s.id = COALESCE(e.sop_id, t.sop_id) AND s.deleted_at IS NULL
	`
}

func buildDataOpsEpisodeWhere(q dataOpsEpisodeQuery) (string, []interface{}) {
	where := " WHERE e.deleted_at IS NULL"
	args := []interface{}{}

	if q.HasCreatedAtFrom {
		where += " AND e.created_at >= ?"
		args = append(args, q.CreatedAtFrom)
	}
	if q.HasCreatedAtTo {
		where += " AND e.created_at <= ?"
		args = append(args, q.CreatedAtTo)
	}

	where, args = appendStringInFilter(where, args, "e.qa_status", q.QAStatuses)
	where, args = appendInt64InFilter(where, args, "e.scene_id", q.SceneIDs)
	where, args = appendInt64InFilter(where, args, "COALESCE(e.sop_id, t.sop_id)", q.SOPIDs)
	where, args = appendInt64InFilter(where, args, "r.robot_type_id", q.RobotTypeIDs)
	where, args = appendStringInFilter(where, args, "COALESCE(NULLIF(r.device_id, ''), NULLIF(ws.robot_serial, ''), '')", q.RobotDeviceIDs)
	where, args = appendStringInFilter(where, args, "COALESCE(NULLIF(dc.operator_id, ''), NULLIF(ws.collector_operator_id, ''), '')", q.CollectorOperatorIDs)

	if q.Keyword != "" {
		where, args = appendKeywordSearch(where, args, q.Keyword, "e.episode_id", "t.task_id", "e.quality_flag")
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
			COALESCE(e.sop_id, t.sop_id) AS sop_id,
			CASE
				WHEN NULLIF(s.slug, '') IS NULL THEN
					CASE
						WHEN COALESCE(e.sop_id, t.sop_id) IS NULL THEN ''
						ELSE CONCAT('SOP #', CAST(COALESCE(e.sop_id, t.sop_id) AS CHAR))
					END
				WHEN NULLIF(s.version, '') IS NULL THEN s.slug
				ELSE CONCAT(s.slug, ' @ ', s.version)
			END AS sop,
			e.scene_id,
			COALESCE(NULLIF(e.scene_name, ''), NULLIF(t.scene_name, ''), NULLIF(sc.name, '')) AS scene_name,
			r.robot_type_id,
				COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), NULLIF(ws.robot_name, '')) AS robot_type,
				COALESCE(NULLIF(r.device_id, ''), NULLIF(ws.robot_serial, '')) AS robot_device_id,
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
			sl.source_factory_id,
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
	return DataOpsEpisodeItemResponse{
		ID:                  row.ID,
		EpisodeID:           row.EpisodeID,
		TaskID:              row.TaskID,
		TaskPublicID:        nullableString(row.TaskPublicID),
		SOPID:               nullableInt64(row.SOPID),
		SOP:                 nullableString(row.SOP),
		SceneID:             row.SceneID,
		SceneName:           nullableString(row.SceneName),
		RobotTypeID:         nullableInt64(row.RobotTypeID),
		RobotType:           nullableString(row.RobotType),
		RobotDeviceID:       nullableString(row.RobotDeviceID),
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
