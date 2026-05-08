// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
)

// DataProductionStatisticsHandler handles Synapse Admin data production statistics APIs.
type DataProductionStatisticsHandler struct {
	db *sqlx.DB
}

// NewDataProductionStatisticsHandler creates a data production statistics handler.
func NewDataProductionStatisticsHandler(db *sqlx.DB) *DataProductionStatisticsHandler {
	return &DataProductionStatisticsHandler{db: db}
}

// RegisterRoutes registers data production statistics routes under /admin/statistics/data-production.
func (h *DataProductionStatisticsHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/summary", h.GetSummary)
	apiV1.GET("/trend", h.GetTrend)
	apiV1.GET("/breakdown", h.GetBreakdown)
	apiV1.GET("/details", h.ListDetails)
	apiV1.GET("/export", h.ExportCSV)
}

type dataProductionStatsQuery struct {
	StartTime            time.Time
	EndTime              time.Time
	Granularity          string
	SourceID             string
	SceneID              int64
	RobotDeviceIDs       []string
	RobotTypeID          string
	CollectorOperatorIDs []string
	SOPID                string
	QAStatus             string
	CloudSynced          *bool
	DataType             string
	TaskID               string
}

type statsCountMetrics struct {
	Total       int64   `json:"total"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	Processing  int64   `json:"processing"`
	SuccessRate float64 `json:"success_rate"`
}

type statsDurationMetrics struct {
	TotalMs int64 `json:"total_ms,omitempty"`
	AvgMs   int64 `json:"avg_ms"`
	P95Ms   int64 `json:"p95_ms"`
	MaxMs   int64 `json:"max_ms"`
}

type statsSizeMetrics struct {
	TotalBytes   int64 `json:"total_bytes"`
	SuccessBytes int64 `json:"success_bytes,omitempty"`
	FailedBytes  int64 `json:"failed_bytes,omitempty"`
	AvgBytes     int64 `json:"avg_bytes"`
	MaxBytes     int64 `json:"max_bytes"`
}

type statsQAMetrics struct {
	Approved int64   `json:"approved"`
	PassRate float64 `json:"pass_rate"`
}

type statsCloudSyncMetrics struct {
	Synced   int64   `json:"synced"`
	Unsynced int64   `json:"unsynced"`
	SyncRate float64 `json:"sync_rate"`
}

type dataProductionSummaryResponse struct {
	Count    statsCountMetrics     `json:"count"`
	Duration statsDurationMetrics  `json:"duration"`
	Size     statsSizeMetrics      `json:"size"`
	QA       statsQAMetrics        `json:"qa"`
	Cloud    statsCloudSyncMetrics `json:"cloud_sync"`
	Compare  *statsCompareMetrics  `json:"compare,omitempty"`
}

type statsCompareMetrics struct {
	PreviousTotalCount int64   `json:"previous_total_count"`
	CountChangeRate    float64 `json:"count_change_rate"`
}

type dataProductionTrendResponse struct {
	Granularity string                    `json:"granularity"`
	Items       []dataProductionTrendItem `json:"items"`
}

type dataProductionTrendItem struct {
	Time     string               `json:"time"`
	Count    statsCountMetrics    `json:"count"`
	Duration statsDurationMetrics `json:"duration"`
	Size     statsSizeMetrics     `json:"size"`
}

type dataProductionBreakdownResponse struct {
	Dimension string                        `json:"dimension"`
	Items     []dataProductionBreakdownItem `json:"items"`
	Total     int64                         `json:"total"`
	Limit     int                           `json:"limit"`
	Offset    int                           `json:"offset"`
	HasNext   bool                          `json:"hasNext,omitempty"`
	HasPrev   bool                          `json:"hasPrev,omitempty"`
}

type dataProductionBreakdownItem struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Count    statsCountMetrics    `json:"count"`
	Duration statsDurationMetrics `json:"duration"`
	Size     statsSizeMetrics     `json:"size"`
}

type dataProductionDetailItem struct {
	ID                  string `json:"id" db:"id"`
	Time                string `json:"time" db:"time"`
	SourceID            string `json:"source_id" db:"source_id"`
	SourceName          string `json:"source_name" db:"source_name"`
	SceneID             string `json:"scene_id" db:"scene_id"`
	SceneName           string `json:"scene_name" db:"scene_name"`
	RobotDeviceID       string `json:"robot_device_id" db:"robot_device_id"`
	RobotTypeID         string `json:"robot_type_id" db:"robot_type_id"`
	RobotTypeName       string `json:"robot_type_name" db:"robot_type_name"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	CollectorName       string `json:"collector_name" db:"collector_name"`
	TaskID              string `json:"task_id" db:"task_id"`
	TaskName            string `json:"task_name" db:"task_name"`
	SOPID               string `json:"sop_id" db:"sop_id"`
	SOP                 string `json:"sop" db:"sop"`
	DataType            string `json:"data_type" db:"data_type"`
	Status              string `json:"status" db:"status"`
	QAStatus            string `json:"qa_status" db:"qa_status"`
	CloudSynced         bool   `json:"cloud_synced" db:"cloud_synced"`
	Count               int64  `json:"count" db:"count_value"`
	DurationMs          *int64 `json:"duration_ms" db:"duration_ms"`
	SizeBytes           *int64 `json:"size_bytes" db:"size_bytes"`
	ErrorCode           string `json:"error_code" db:"error_code"`
	ErrorMessage        string `json:"error_message" db:"error_message"`
}

type dataProductionDetailsResponse struct {
	Items   []dataProductionDetailItem `json:"items"`
	Total   int64                      `json:"total"`
	Limit   int                        `json:"limit"`
	Offset  int                        `json:"offset"`
	HasNext bool                       `json:"hasNext,omitempty"`
	HasPrev bool                       `json:"hasPrev,omitempty"`
}

type aggregateStatsRow struct {
	TotalCount      sql.NullInt64   `db:"total_count"`
	SuccessCount    sql.NullInt64   `db:"success_count"`
	FailedCount     sql.NullInt64   `db:"failed_count"`
	ProcessingCount sql.NullInt64   `db:"processing_count"`
	ApprovedQACount sql.NullInt64   `db:"approved_qa_count"`
	CloudSynced     sql.NullInt64   `db:"cloud_synced_count"`
	CloudUnsynced   sql.NullInt64   `db:"cloud_unsynced_count"`
	TotalDurationMs sql.NullFloat64 `db:"total_duration_ms"`
	AvgDurationMs   sql.NullFloat64 `db:"avg_duration_ms"`
	MaxDurationMs   sql.NullFloat64 `db:"max_duration_ms"`
	TotalBytes      sql.NullInt64   `db:"total_bytes"`
	SuccessBytes    sql.NullInt64   `db:"success_bytes"`
	FailedBytes     sql.NullInt64   `db:"failed_bytes"`
	AvgBytes        sql.NullFloat64 `db:"avg_bytes"`
	MaxBytes        sql.NullInt64   `db:"max_bytes"`
}

type trendStatsRow struct {
	Bucket          string          `db:"bucket"`
	TotalCount      sql.NullInt64   `db:"total_count"`
	SuccessCount    sql.NullInt64   `db:"success_count"`
	FailedCount     sql.NullInt64   `db:"failed_count"`
	ProcessingCount sql.NullInt64   `db:"processing_count"`
	ApprovedQACount sql.NullInt64   `db:"approved_qa_count"`
	CloudSynced     sql.NullInt64   `db:"cloud_synced_count"`
	CloudUnsynced   sql.NullInt64   `db:"cloud_unsynced_count"`
	TotalDurationMs sql.NullFloat64 `db:"total_duration_ms"`
	AvgDurationMs   sql.NullFloat64 `db:"avg_duration_ms"`
	MaxDurationMs   sql.NullFloat64 `db:"max_duration_ms"`
	TotalBytes      sql.NullInt64   `db:"total_bytes"`
	AvgBytes        sql.NullFloat64 `db:"avg_bytes"`
	MaxBytes        sql.NullInt64   `db:"max_bytes"`
}

type breakdownStatsRow struct {
	ID              sql.NullString  `db:"id"`
	Name            sql.NullString  `db:"name"`
	TotalCount      sql.NullInt64   `db:"total_count"`
	SuccessCount    sql.NullInt64   `db:"success_count"`
	FailedCount     sql.NullInt64   `db:"failed_count"`
	ProcessingCount sql.NullInt64   `db:"processing_count"`
	AvgDurationMs   sql.NullFloat64 `db:"avg_duration_ms"`
	MaxDurationMs   sql.NullFloat64 `db:"max_duration_ms"`
	TotalBytes      sql.NullInt64   `db:"total_bytes"`
	AvgBytes        sql.NullFloat64 `db:"avg_bytes"`
	MaxBytes        sql.NullInt64   `db:"max_bytes"`
}

var validStatsGranularities = map[string]struct{}{
	"hour":  {},
	"day":   {},
	"week":  {},
	"month": {},
}

var validDataProductionQAStatuses = map[string]struct{}{
	"pending_qa":         {},
	"qa_running":         {},
	"approved":           {},
	"needs_inspection":   {},
	"inspector_approved": {},
	"rejected":           {},
	"failed":             {},
}

func parseDataProductionStatsQuery(c *gin.Context, requireGranularity bool) (dataProductionStatsQuery, error) {
	startTime, err := parseStatsTime(c.Query("start_time"))
	if err != nil {
		return dataProductionStatsQuery{}, fmt.Errorf("start_time is required and must be RFC3339")
	}
	endTime, err := parseStatsTime(c.Query("end_time"))
	if err != nil {
		return dataProductionStatsQuery{}, fmt.Errorf("end_time is required and must be RFC3339")
	}
	if !endTime.After(startTime) {
		return dataProductionStatsQuery{}, fmt.Errorf("end_time must be after start_time")
	}

	granularity := strings.TrimSpace(c.DefaultQuery("granularity", "day"))
	if requireGranularity {
		if _, ok := validStatsGranularities[granularity]; !ok {
			return dataProductionStatsQuery{}, fmt.Errorf("granularity must be one of hour, day, week, month")
		}
	}

	var sceneID int64
	rawSceneID := strings.TrimSpace(c.Query("scene_id"))
	if rawSceneID != "" {
		parsedSceneID, err := strconv.ParseInt(rawSceneID, 10, 64)
		if err != nil || parsedSceneID <= 0 {
			return dataProductionStatsQuery{}, fmt.Errorf("scene_id must be a positive integer")
		}
		sceneID = parsedSceneID
	}

	qaStatus := strings.TrimSpace(c.Query("qa_status"))
	if qaStatus != "" {
		if _, ok := validDataProductionQAStatuses[qaStatus]; !ok {
			return dataProductionStatsQuery{}, fmt.Errorf("qa_status must be one of pending_qa, qa_running, approved, needs_inspection, inspector_approved, rejected, failed")
		}
	}

	var cloudSynced *bool
	rawCloudSynced := strings.TrimSpace(c.Query("cloud_synced"))
	if rawCloudSynced != "" {
		parsedCloudSynced, err := strconv.ParseBool(rawCloudSynced)
		if err != nil {
			return dataProductionStatsQuery{}, fmt.Errorf("cloud_synced must be true or false")
		}
		cloudSynced = &parsedCloudSynced
	}

	return dataProductionStatsQuery{
		StartTime:            startTime,
		EndTime:              endTime,
		Granularity:          granularity,
		SourceID:             strings.TrimSpace(c.Query("source_id")),
		SceneID:              sceneID,
		RobotDeviceIDs:       parseStatsStringListQuery(c, "robot_device_id"),
		RobotTypeID:          strings.TrimSpace(c.Query("robot_type_id")),
		CollectorOperatorIDs: parseStatsStringListQuery(c, "collector_operator_id"),
		SOPID:                strings.TrimSpace(c.Query("sop_id")),
		QAStatus:             qaStatus,
		CloudSynced:          cloudSynced,
		DataType:             strings.TrimSpace(c.Query("data_type")),
		TaskID:               strings.TrimSpace(c.Query("task_id")),
	}, nil
}

func parseStatsTime(raw string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp")
}

func parseStatsStringListQuery(c *gin.Context, key string) []string {
	seen := map[string]struct{}{}
	values := []string{}

	for _, rawValue := range c.QueryArray(key) {
		for _, part := range strings.Split(rawValue, ",") {
			value := strings.TrimSpace(part)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}

	return values
}

// GetSummary returns aggregate data production statistics for the requested filters.
func (h *DataProductionStatisticsHandler) GetSummary(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	row, err := h.aggregateStats(q, "")
	if err != nil {
		logger.Printf("[DATA_STATS] summary query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production summary"})
		return
	}

	p95, err := h.percentileDuration(q, "", 0.95)
	if err != nil {
		logger.Printf("[DATA_STATS] summary p95 query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production summary"})
		return
	}

	previousTotal, changeRate, err := h.previousPeriodCompare(q)
	if err != nil {
		logger.Printf("[DATA_STATS] summary compare query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production summary"})
		return
	}

	resp := aggregateRowToSummary(row, p95)
	resp.Compare = &statsCompareMetrics{
		PreviousTotalCount: previousTotal,
		CountChangeRate:    changeRate,
	}
	c.JSON(http.StatusOK, resp)
}

// GetTrend returns bucketed data production statistics for chart rendering.
func (h *DataProductionStatisticsHandler) GetTrend(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	timezoneOffset, err := parseStatsTimezoneOffset(c.Query("timezone_offset"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	bucketExpr, bucketArgs := statsBucketExpression(q.Granularity, timezoneOffset)
	baseSQL, args := h.filteredProductionRecordsSQL(q)
	query := fmt.Sprintf(`
		SELECT
			%s AS bucket,
			COALESCE(SUM(count_value), 0) AS total_count,
			COALESCE(SUM(CASE WHEN status = 'success' THEN count_value ELSE 0 END), 0) AS success_count,
			COALESCE(SUM(CASE WHEN status IN ('failed', 'cancelled') THEN count_value ELSE 0 END), 0) AS failed_count,
			COALESCE(SUM(CASE WHEN status = 'processing' THEN count_value ELSE 0 END), 0) AS processing_count,
			COALESCE(SUM(CASE WHEN qa_status IN ('approved', 'inspector_approved') THEN count_value ELSE 0 END), 0) AS approved_qa_count,
			COALESCE(SUM(CASE WHEN cloud_synced THEN count_value ELSE 0 END), 0) AS cloud_synced_count,
			COALESCE(SUM(CASE WHEN NOT COALESCE(cloud_synced, FALSE) THEN count_value ELSE 0 END), 0) AS cloud_unsynced_count,
			SUM(duration_ms) AS total_duration_ms,
			AVG(duration_ms) AS avg_duration_ms,
			MAX(duration_ms) AS max_duration_ms,
			COALESCE(SUM(COALESCE(size_bytes, 0)), 0) AS total_bytes,
			AVG(size_bytes) AS avg_bytes,
			MAX(size_bytes) AS max_bytes
		FROM (%s) p
		GROUP BY bucket
		ORDER BY bucket ASC
	`, bucketExpr, baseSQL)

	var rows []trendStatsRow
	queryArgs := append(bucketArgs, args...)
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[DATA_STATS] trend query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production trend"})
		return
	}

	items := make([]dataProductionTrendItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, trendRowToItem(row))
	}
	c.JSON(http.StatusOK, dataProductionTrendResponse{Granularity: q.Granularity, Items: items})
}

// GetBreakdown returns paginated statistics grouped by the requested dimension.
func (h *DataProductionStatisticsHandler) GetBreakdown(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	dimension := strings.TrimSpace(c.DefaultQuery("dimension", "source"))
	idExpr, nameExpr, err := statsBreakdownExpressions(dimension)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	baseSQL, args := h.filteredProductionRecordsSQL(q)
	countQuery := dataProductionBreakdownCountSQL(idExpr, baseSQL)

	var total int64
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[DATA_STATS] breakdown count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production breakdown"})
		return
	}

	query := dataProductionBreakdownSQL(idExpr, nameExpr, baseSQL)

	var rows []breakdownStatsRow
	queryArgs := append(args, pagination.Limit, pagination.Offset)
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[DATA_STATS] breakdown query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production breakdown"})
		return
	}

	items := make([]dataProductionBreakdownItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, breakdownRowToItem(row))
	}
	c.JSON(http.StatusOK, dataProductionBreakdownResponse{
		Dimension: dimension,
		Items:     items,
		Total:     total,
		Limit:     pagination.Limit,
		Offset:    pagination.Offset,
		HasNext:   int64(pagination.Offset+pagination.Limit) < total,
		HasPrev:   pagination.Offset > 0,
	})
}

func dataProductionBreakdownCountSQL(idExpr string, baseSQL string) string {
	return fmt.Sprintf(`
		SELECT COUNT(1)
		FROM (
			SELECT %s AS id
			FROM (%s) p
			GROUP BY %s
		) grouped
	`, idExpr, baseSQL, idExpr)
}

func dataProductionBreakdownSQL(idExpr string, nameExpr string, baseSQL string) string {
	return fmt.Sprintf(`
		SELECT
			%s AS id,
			MAX(%s) AS name,
			COALESCE(SUM(count_value), 0) AS total_count,
			COALESCE(SUM(CASE WHEN status = 'success' THEN count_value ELSE 0 END), 0) AS success_count,
			COALESCE(SUM(CASE WHEN status IN ('failed', 'cancelled') THEN count_value ELSE 0 END), 0) AS failed_count,
			COALESCE(SUM(CASE WHEN status = 'processing' THEN count_value ELSE 0 END), 0) AS processing_count,
			AVG(duration_ms) AS avg_duration_ms,
			MAX(duration_ms) AS max_duration_ms,
			COALESCE(SUM(COALESCE(size_bytes, 0)), 0) AS total_bytes,
			AVG(size_bytes) AS avg_bytes,
			MAX(size_bytes) AS max_bytes
		FROM (%s) p
		GROUP BY %s
		ORDER BY total_count DESC, failed_count DESC, id ASC
		LIMIT ? OFFSET ?
	`, idExpr, nameExpr, baseSQL, idExpr)
}

// ListDetails returns paginated episode-level records backing the statistics.
func (h *DataProductionStatisticsHandler) ListDetails(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	sortBy, sortOrder, err := parseStatsDetailSort(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	baseSQL, args := h.filteredProductionRecordsSQL(q)
	var total int64
	if err := h.db.Get(&total, fmt.Sprintf("SELECT COUNT(1) FROM (%s) p", baseSQL), args...); err != nil {
		logger.Printf("[DATA_STATS] detail count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data production details"})
		return
	}

	query := dataProductionDetailsSQL(baseSQL, sortBy, sortOrder)
	queryArgs := append(args, pagination.Limit, pagination.Offset)

	var items []dataProductionDetailItem
	if err := h.db.Select(&items, query, queryArgs...); err != nil {
		logger.Printf("[DATA_STATS] detail query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data production details"})
		return
	}

	c.JSON(http.StatusOK, dataProductionDetailsResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: int64(pagination.Offset+pagination.Limit) < total,
		HasPrev: pagination.Offset > 0,
	})
}

func dataProductionDetailsSQL(baseSQL string, sortBy string, sortOrder string) string {
	return fmt.Sprintf(`
		SELECT
			id,
			DATE_FORMAT(COALESCE(CONVERT_TZ(event_time, @@session.time_zone, '+00:00'), event_time), '%%Y-%%m-%%dT%%H:%%i:%%sZ') AS time,
			source_id,
			source_name,
			scene_id,
			scene_name,
			robot_device_id,
			robot_type_id,
			robot_type_name,
			collector_operator_id,
			collector_name,
			task_id,
			task_name,
			sop_id,
			sop,
			data_type,
			status,
			qa_status,
			COALESCE(cloud_synced, FALSE) AS cloud_synced,
			count_value,
			duration_ms,
			size_bytes,
			error_code,
			error_message
		FROM (%s) p
		ORDER BY %s %s
		LIMIT ? OFFSET ?
	`, baseSQL, sortBy, sortOrder)
}

// ExportCSV streams filtered data production detail records as CSV.
func (h *DataProductionStatisticsHandler) ExportCSV(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	timezoneOffset, err := parseStatsTimezoneOffset(c.Query("timezone_offset"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	baseSQL, args := h.filteredProductionRecordsSQL(q)
	query := fmt.Sprintf(`
		SELECT
			episode_id AS id,
			DATE_FORMAT(COALESCE(CONVERT_TZ(event_time, @@session.time_zone, ?), event_time), '%%Y-%%m-%%d %%H:%%i:%%s') AS time,
			robot_device_id,
			robot_type_name,
			collector_operator_id,
			collector_name,
			task_id,
			sop,
			duration_ms,
			size_bytes
		FROM (%s) p
		ORDER BY event_time DESC
		LIMIT 10000
	`, baseSQL)

	var items []dataProductionDetailItem
	queryArgs := append([]interface{}{timezoneOffset}, args...)
	if err := h.db.Select(&items, query, queryArgs...); err != nil {
		logger.Printf("[DATA_STATS] export query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to export data production details"})
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="data-production-statistics.csv"`)

	writer := csv.NewWriter(c.Writer)
	_ = writer.Write([]string{
		"id", "time", "设备ID", "设备型号", "数采员工号", "数采员姓名", "task_id", "sop", "时长", "大小",
	})
	for _, item := range items {
		_ = writer.Write([]string{
			item.ID,
			item.Time,
			item.RobotDeviceID,
			item.RobotTypeName,
			item.CollectorOperatorID,
			item.CollectorName,
			item.TaskID,
			item.SOP,
			formatExportDuration(item.DurationMs),
			formatExportSize(item.SizeBytes),
		})
	}
	writer.Flush()
}

func (h *DataProductionStatisticsHandler) aggregateStats(q dataProductionStatsQuery, extraWhere string, extraArgs ...interface{}) (aggregateStatsRow, error) {
	baseSQL, args := h.filteredProductionRecordsSQL(q)
	args = append(args, extraArgs...)
	query := fmt.Sprintf(`
		SELECT
			COALESCE(SUM(count_value), 0) AS total_count,
			COALESCE(SUM(CASE WHEN status = 'success' THEN count_value ELSE 0 END), 0) AS success_count,
			COALESCE(SUM(CASE WHEN status IN ('failed', 'cancelled') THEN count_value ELSE 0 END), 0) AS failed_count,
			COALESCE(SUM(CASE WHEN status = 'processing' THEN count_value ELSE 0 END), 0) AS processing_count,
			COALESCE(SUM(CASE WHEN qa_status IN ('approved', 'inspector_approved') THEN count_value ELSE 0 END), 0) AS approved_qa_count,
			COALESCE(SUM(CASE WHEN cloud_synced THEN count_value ELSE 0 END), 0) AS cloud_synced_count,
			COALESCE(SUM(CASE WHEN NOT COALESCE(cloud_synced, FALSE) THEN count_value ELSE 0 END), 0) AS cloud_unsynced_count,
			SUM(duration_ms) AS total_duration_ms,
			AVG(duration_ms) AS avg_duration_ms,
			MAX(duration_ms) AS max_duration_ms,
			COALESCE(SUM(COALESCE(size_bytes, 0)), 0) AS total_bytes,
			COALESCE(SUM(CASE WHEN status = 'success' THEN COALESCE(size_bytes, 0) ELSE 0 END), 0) AS success_bytes,
			COALESCE(SUM(CASE WHEN status IN ('failed', 'cancelled') THEN COALESCE(size_bytes, 0) ELSE 0 END), 0) AS failed_bytes,
			AVG(size_bytes) AS avg_bytes,
			MAX(size_bytes) AS max_bytes
		FROM (%s) p
		%s
	`, baseSQL, extraWhere)

	var row aggregateStatsRow
	err := h.db.Get(&row, query, args...)
	return row, err
}

func (h *DataProductionStatisticsHandler) previousPeriodCompare(q dataProductionStatsQuery) (int64, float64, error) {
	span := q.EndTime.Sub(q.StartTime)
	previous := q
	previous.EndTime = q.StartTime
	previous.StartTime = q.StartTime.Add(-span)

	row, err := h.aggregateStats(previous, "")
	if err != nil {
		return 0, 0, err
	}
	previousTotal := nullInt64(row.TotalCount)
	currentRow, err := h.aggregateStats(q, "")
	if err != nil {
		return 0, 0, err
	}
	currentTotal := nullInt64(currentRow.TotalCount)
	if previousTotal == 0 {
		if currentTotal == 0 {
			return previousTotal, 0, nil
		}
		return previousTotal, 1, nil
	}
	return previousTotal, float64(currentTotal-previousTotal) / float64(previousTotal), nil
}

func (h *DataProductionStatisticsHandler) percentileDuration(q dataProductionStatsQuery, extraWhere string, percentile float64, extraArgs ...interface{}) (int64, error) {
	baseSQL, args := h.filteredProductionRecordsSQL(q)
	countArgs := append([]interface{}{}, args...)
	countArgs = append(countArgs, extraArgs...)
	countQuery := fmt.Sprintf("SELECT COUNT(1) FROM (%s) p WHERE duration_ms IS NOT NULL %s", baseSQL, extraWhere)

	var count int64
	if err := h.db.Get(&count, countQuery, countArgs...); err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	offset := int64(math.Ceil(float64(count)*percentile)) - 1
	if offset < 0 {
		offset = 0
	}

	valueArgs := append([]interface{}{}, args...)
	valueArgs = append(valueArgs, extraArgs...)
	valueArgs = append(valueArgs, 1, offset)
	valueQuery := fmt.Sprintf(`
		SELECT duration_ms
		FROM (%s) p
		WHERE duration_ms IS NOT NULL %s
		ORDER BY duration_ms ASC
		LIMIT ? OFFSET ?
	`, baseSQL, extraWhere)

	var value sql.NullFloat64
	if err := h.db.Get(&value, valueQuery, valueArgs...); err != nil {
		return 0, err
	}
	return roundNullFloat(value), nil
}

func parseStatsTimezoneOffset(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "+00:00", nil
	}
	if len(value) != 6 || (value[0] != '+' && value[0] != '-') || value[3] != ':' {
		return "", fmt.Errorf("timezone_offset must be formatted as +08:00 or -05:00")
	}

	hours, err := strconv.Atoi(value[1:3])
	if err != nil {
		return "", fmt.Errorf("timezone_offset must be formatted as +08:00 or -05:00")
	}
	minutes, err := strconv.Atoi(value[4:6])
	if err != nil {
		return "", fmt.Errorf("timezone_offset must be formatted as +08:00 or -05:00")
	}
	if hours > 14 || minutes > 59 || (hours == 14 && minutes != 0) {
		return "", fmt.Errorf("timezone_offset is out of range")
	}

	return value, nil
}

func (h *DataProductionStatisticsHandler) filteredProductionRecordsSQL(q dataProductionStatsQuery) (string, []interface{}) {
	baseSQL := productionRecordsSQL()
	conditions := []string{"event_time >= ?", "event_time < ?"}
	args := []interface{}{q.StartTime, q.EndTime}

	if q.SourceID != "" {
		conditions = append(conditions, "source_id = ?")
		args = append(args, q.SourceID)
	}
	if q.SceneID > 0 {
		conditions = append(conditions, "scene_id = ?")
		args = append(args, q.SceneID)
	}
	conditions, args = appendStatsInCondition(conditions, args, "robot_device_id", q.RobotDeviceIDs)
	if q.RobotTypeID != "" {
		conditions = append(conditions, "robot_type_id = ?")
		args = append(args, q.RobotTypeID)
	}
	conditions, args = appendStatsInCondition(conditions, args, "collector_operator_id", q.CollectorOperatorIDs)
	if q.SOPID != "" {
		conditions = append(conditions, "sop_id = ?")
		args = append(args, q.SOPID)
	}
	if q.QAStatus != "" {
		conditions = append(conditions, "qa_status = ?")
		args = append(args, q.QAStatus)
	}
	if q.CloudSynced != nil {
		conditions = append(conditions, "cloud_synced = ?")
		args = append(args, *q.CloudSynced)
	}
	if q.DataType != "" {
		conditions = append(conditions, "data_type = ?")
		args = append(args, q.DataType)
	}
	if q.TaskID != "" {
		conditions = append(conditions, "task_id = ?")
		args = append(args, q.TaskID)
	}

	query := fmt.Sprintf("SELECT * FROM (%s) production_records WHERE %s", baseSQL, strings.Join(conditions, " AND "))
	return query, args
}

func appendStatsInCondition(conditions []string, args []interface{}, column string, values []string) ([]string, []interface{}) {
	if len(values) == 0 {
		return conditions, args
	}

	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	conditions = append(conditions, fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ",")))
	return conditions, args
}

func productionRecordsSQL() string {
	return `
		SELECT
			CONCAT('episode:', e.id) AS id,
			COALESCE(e.episode_id, '') AS episode_id,
			COALESCE(t.completed_at, e.created_at) AS event_time,
			COALESCE(r.device_id, ws.robot_serial, CAST(COALESCE(e.workstation_id, t.workstation_id) AS CHAR), '') AS source_id,
			COALESCE(r.device_id, ws.robot_name, ws.name, CONCAT('workstation:', CAST(COALESCE(e.workstation_id, t.workstation_id) AS CHAR)), 'unknown') AS source_name,
			e.scene_id AS scene_id,
			COALESCE(e.scene_name, t.scene_name, '') AS scene_name,
			COALESCE(r.device_id, ws.robot_serial, '') AS robot_device_id,
			COALESCE(CAST(r.robot_type_id AS CHAR), '') AS robot_type_id,
			COALESCE(NULLIF(rt.name, ''), NULLIF(ws.robot_name, ''), '') AS robot_type_name,
			COALESCE(dc.operator_id, ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(dc.name, ws.collector_name, '') AS collector_name,
			COALESCE(t.task_id, CAST(e.task_id AS CHAR)) AS task_id,
			COALESCE(t.task_id, CAST(e.task_id AS CHAR)) AS task_name,
			COALESCE(CAST(e.sop_id AS CHAR), CAST(t.sop_id AS CHAR), '') AS sop_id,
			CASE
				WHEN NULLIF(s.slug, '') IS NULL THEN
					CASE
						WHEN COALESCE(e.sop_id, t.sop_id) IS NULL THEN ''
						ELSE CONCAT('SOP #', CAST(COALESCE(e.sop_id, t.sop_id) AS CHAR))
					END
				WHEN NULLIF(s.version, '') IS NULL THEN s.slug
				ELSE CONCAT(s.slug, ' @ ', s.version)
			END AS sop,
			'episode' AS data_type,
			'success' AS status,
			COALESCE(e.qa_status, '') AS qa_status,
			e.cloud_synced AS cloud_synced,
			1 AS count_value,
			CAST(COALESCE(e.duration_sec * 1000, TIMESTAMPDIFF(MICROSECOND, t.started_at, t.completed_at) / 1000) AS SIGNED) AS duration_ms,
			e.file_size_bytes AS size_bytes,
			'' AS error_code,
			'' AS error_message
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		LEFT JOIN sops s ON s.id = COALESCE(e.sop_id, t.sop_id) AND s.deleted_at IS NULL
		WHERE e.deleted_at IS NULL
	`
}

func aggregateRowToSummary(row aggregateStatsRow, p95 int64) dataProductionSummaryResponse {
	total := nullInt64(row.TotalCount)
	success := nullInt64(row.SuccessCount)
	approvedQA := nullInt64(row.ApprovedQACount)
	cloudSynced := nullInt64(row.CloudSynced)
	return dataProductionSummaryResponse{
		Count: statsCountMetrics{
			Total:       total,
			Success:     success,
			Failed:      nullInt64(row.FailedCount),
			Processing:  nullInt64(row.ProcessingCount),
			SuccessRate: rate(success, total),
		},
		Duration: statsDurationMetrics{
			TotalMs: roundNullFloat(row.TotalDurationMs),
			AvgMs:   roundNullFloat(row.AvgDurationMs),
			P95Ms:   p95,
			MaxMs:   roundNullFloat(row.MaxDurationMs),
		},
		Size: statsSizeMetrics{
			TotalBytes:   nullInt64(row.TotalBytes),
			SuccessBytes: nullInt64(row.SuccessBytes),
			FailedBytes:  nullInt64(row.FailedBytes),
			AvgBytes:     roundNullFloat(row.AvgBytes),
			MaxBytes:     nullInt64(row.MaxBytes),
		},
		QA: statsQAMetrics{
			Approved: approvedQA,
			PassRate: rate(approvedQA, total),
		},
		Cloud: statsCloudSyncMetrics{
			Synced:   cloudSynced,
			Unsynced: nullInt64(row.CloudUnsynced),
			SyncRate: rate(cloudSynced, total),
		},
	}
}

func trendRowToItem(row trendStatsRow) dataProductionTrendItem {
	total := nullInt64(row.TotalCount)
	success := nullInt64(row.SuccessCount)
	return dataProductionTrendItem{
		Time: row.Bucket,
		Count: statsCountMetrics{
			Total:       total,
			Success:     success,
			Failed:      nullInt64(row.FailedCount),
			Processing:  nullInt64(row.ProcessingCount),
			SuccessRate: rate(success, total),
		},
		Duration: statsDurationMetrics{
			TotalMs: roundNullFloat(row.TotalDurationMs),
			AvgMs:   roundNullFloat(row.AvgDurationMs),
			MaxMs:   roundNullFloat(row.MaxDurationMs),
		},
		Size: statsSizeMetrics{
			TotalBytes: nullInt64(row.TotalBytes),
			AvgBytes:   roundNullFloat(row.AvgBytes),
			MaxBytes:   nullInt64(row.MaxBytes),
		},
	}
}

func breakdownRowToItem(row breakdownStatsRow) dataProductionBreakdownItem {
	total := nullInt64(row.TotalCount)
	success := nullInt64(row.SuccessCount)
	return dataProductionBreakdownItem{
		ID:   nullString(row.ID),
		Name: nullString(row.Name),
		Count: statsCountMetrics{
			Total:       total,
			Success:     success,
			Failed:      nullInt64(row.FailedCount),
			Processing:  nullInt64(row.ProcessingCount),
			SuccessRate: rate(success, total),
		},
		Duration: statsDurationMetrics{
			AvgMs: roundNullFloat(row.AvgDurationMs),
			MaxMs: roundNullFloat(row.MaxDurationMs),
		},
		Size: statsSizeMetrics{
			TotalBytes: nullInt64(row.TotalBytes),
			AvgBytes:   roundNullFloat(row.AvgBytes),
			MaxBytes:   nullInt64(row.MaxBytes),
		},
	}
}

func statsBucketExpression(granularity string, timezoneOffset string) (string, []interface{}) {
	localEvent := "COALESCE(CONVERT_TZ(event_time, @@session.time_zone, ?), event_time)"
	utcBucket := func(localBucket string, argCount int) (string, []interface{}) {
		expr := fmt.Sprintf(
			"DATE_FORMAT(TIMESTAMPADD(MINUTE, ?, %s), '%%Y-%%m-%%dT%%H:%%i:%%sZ')",
			localBucket,
		)
		args := make([]interface{}, 0, argCount+1)
		args = append(args, -statsTimezoneOffsetMinutes(timezoneOffset))
		for i := 0; i < argCount; i++ {
			args = append(args, timezoneOffset)
		}
		return expr, args
	}

	switch granularity {
	case "hour":
		return utcBucket(fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:00:00')", localEvent), 1)
	case "week":
		localBucket := fmt.Sprintf(
			"DATE_FORMAT(DATE_SUB(DATE(%s), INTERVAL WEEKDAY(%s) DAY), '%%Y-%%m-%%d 00:00:00')",
			localEvent,
			localEvent,
		)
		return utcBucket(localBucket, 2)
	case "month":
		return utcBucket(fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-01 00:00:00')", localEvent), 1)
	default:
		return utcBucket(fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d 00:00:00')", localEvent), 1)
	}
}

func statsTimezoneOffsetMinutes(timezoneOffset string) int {
	sign := 1
	if strings.HasPrefix(timezoneOffset, "-") {
		sign = -1
	}
	hours, _ := strconv.Atoi(timezoneOffset[1:3])
	minutes, _ := strconv.Atoi(timezoneOffset[4:6])
	return sign * (hours*60 + minutes)
}

func statsBreakdownExpressions(dimension string) (string, string, error) {
	switch dimension {
	case "source":
		return "source_id", "source_name", nil
	case "robot_device":
		return "robot_device_id", "robot_device_id", nil
	case "collector":
		return "collector_operator_id", "collector_operator_id", nil
	case "robot_type":
		return "robot_type_id", "robot_type_name", nil
	case "scene":
		return "scene_id", "scene_name", nil
	case "sop":
		return "sop_id", "sop", nil
	case "qa_status":
		return "qa_status", `CASE qa_status
			WHEN 'pending_qa' THEN '待质检'
			WHEN 'qa_running' THEN '质检中'
			WHEN 'approved' THEN '已通过'
			WHEN 'needs_inspection' THEN '需人工复核'
			WHEN 'inspector_approved' THEN '人工通过'
			WHEN 'rejected' THEN '已驳回'
			WHEN 'failed' THEN '质检失败'
			ELSE qa_status
		END`, nil
	case "cloud_synced":
		return "CASE WHEN cloud_synced THEN 'true' ELSE 'false' END", "CASE WHEN cloud_synced THEN '已同步' ELSE '未同步' END", nil
	default:
		return "", "", fmt.Errorf("dimension must be one of source, robot_device, collector, robot_type, scene, sop, qa_status, cloud_synced")
	}
}

func parseStatsDetailSort(c *gin.Context) (string, string, error) {
	sortFields := map[string]string{
		"time":        "event_time",
		"count":       "count_value",
		"duration_ms": "duration_ms",
		"size_bytes":  "size_bytes",
		"status":      "status",
	}
	sortBy := sortFields[strings.TrimSpace(c.DefaultQuery("sort_by", "time"))]
	if sortBy == "" {
		return "", "", fmt.Errorf("sort_by must be one of time, count, duration_ms, size_bytes, status")
	}
	sortOrder := strings.ToUpper(strings.TrimSpace(c.DefaultQuery("sort_order", "desc")))
	if sortOrder != "ASC" && sortOrder != "DESC" {
		return "", "", fmt.Errorf("sort_order must be asc or desc")
	}
	return sortBy, sortOrder, nil
}

func rate(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func nullInt64(v sql.NullInt64) int64 {
	if !v.Valid {
		return 0
	}
	return v.Int64
}

func nullString(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func roundNullFloat(v sql.NullFloat64) int64 {
	if !v.Valid {
		return 0
	}
	return int64(math.Round(v.Float64))
}

func formatExportDuration(value *int64) string {
	if value == nil {
		return ""
	}
	seconds := float64(*value) / 1000
	return fmt.Sprintf("%.2f秒 (%.2fh)", seconds, seconds/3600)
}

func formatExportSize(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d 字节 (%s)", *value, formatDecimalBytes(*value))
}

func formatDecimalBytes(bytes int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(bytes)
	unitIndex := 0
	for value >= 1000 && unitIndex < len(units)-1 {
		value /= 1000
		unitIndex++
	}
	if unitIndex == 0 {
		return fmt.Sprintf("%.0f%s", value, units[unitIndex])
	}
	return fmt.Sprintf("%.2f%s", value, units[unitIndex])
}
