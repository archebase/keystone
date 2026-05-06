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
	StartTime           time.Time
	EndTime             time.Time
	Granularity         string
	SourceID            string
	RobotDeviceID       string
	CollectorOperatorID string
	DataType            string
	Status              string
	TaskID              string
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

type dataProductionSummaryResponse struct {
	Count    statsCountMetrics    `json:"count"`
	Duration statsDurationMetrics `json:"duration"`
	Size     statsSizeMetrics     `json:"size"`
	Compare  *statsCompareMetrics `json:"compare,omitempty"`
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
	RobotDeviceID       string `json:"robot_device_id" db:"robot_device_id"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	TaskID              string `json:"task_id" db:"task_id"`
	TaskName            string `json:"task_name" db:"task_name"`
	DataType            string `json:"data_type" db:"data_type"`
	Status              string `json:"status" db:"status"`
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

var validDataProductionStatuses = map[string]struct{}{
	"success":    {},
	"failed":     {},
	"cancelled":  {},
	"processing": {},
}

var validStatsGranularities = map[string]struct{}{
	"hour":  {},
	"day":   {},
	"week":  {},
	"month": {},
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

	status := strings.TrimSpace(c.Query("status"))
	if status != "" {
		if _, ok := validDataProductionStatuses[status]; !ok {
			return dataProductionStatsQuery{}, fmt.Errorf("status must be one of success, failed, cancelled, processing")
		}
	}

	return dataProductionStatsQuery{
		StartTime:           startTime,
		EndTime:             endTime,
		Granularity:         granularity,
		SourceID:            strings.TrimSpace(c.Query("source_id")),
		RobotDeviceID:       strings.TrimSpace(c.Query("robot_device_id")),
		CollectorOperatorID: strings.TrimSpace(c.Query("collector_operator_id")),
		DataType:            strings.TrimSpace(c.Query("data_type")),
		Status:              status,
		TaskID:              strings.TrimSpace(c.Query("task_id")),
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

func (h *DataProductionStatisticsHandler) GetTrend(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, true)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	bucketExpr := statsBucketExpression(q.Granularity)
	baseSQL, args := h.filteredProductionRecordsSQL(q)
	query := fmt.Sprintf(`
		SELECT
			%s AS bucket,
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
		GROUP BY bucket
		ORDER BY bucket ASC
	`, bucketExpr, baseSQL)

	var rows []trendStatsRow
	if err := h.db.Select(&rows, query, args...); err != nil {
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

func (h *DataProductionStatisticsHandler) GetBreakdown(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dimension := strings.TrimSpace(c.DefaultQuery("dimension", "source"))
	idExpr, nameExpr, err := statsBreakdownExpressions(dimension)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	baseSQL, args := h.filteredProductionRecordsSQL(q)
	query := fmt.Sprintf(`
		SELECT
			%s AS id,
			%s AS name,
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
		GROUP BY id, name
		ORDER BY total_count DESC, failed_count DESC
		LIMIT 100
	`, idExpr, nameExpr, baseSQL)

	var rows []breakdownStatsRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[DATA_STATS] breakdown query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data production breakdown"})
		return
	}

	items := make([]dataProductionBreakdownItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, breakdownRowToItem(row))
	}
	c.JSON(http.StatusOK, dataProductionBreakdownResponse{Dimension: dimension, Items: items})
}

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

	query := fmt.Sprintf(`
		SELECT
			id,
			DATE_FORMAT(CONVERT_TZ(event_time, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') AS time,
			source_id,
			source_name,
			robot_device_id,
			collector_operator_id,
			task_id,
			task_name,
			data_type,
			status,
			count_value,
			duration_ms,
			size_bytes,
			error_code,
			error_message
		FROM (%s) p
		ORDER BY %s %s
		LIMIT ? OFFSET ?
	`, baseSQL, sortBy, sortOrder)
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

func (h *DataProductionStatisticsHandler) ExportCSV(c *gin.Context) {
	q, err := parseDataProductionStatsQuery(c, false)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	baseSQL, args := h.filteredProductionRecordsSQL(q)
	query := fmt.Sprintf(`
		SELECT
			id,
			DATE_FORMAT(CONVERT_TZ(event_time, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') AS time,
			source_id,
			source_name,
			robot_device_id,
			collector_operator_id,
			task_id,
			task_name,
			data_type,
			status,
			count_value,
			duration_ms,
			size_bytes,
			error_code,
			error_message
		FROM (%s) p
		ORDER BY event_time DESC
		LIMIT 10000
	`, baseSQL)

	var items []dataProductionDetailItem
	if err := h.db.Select(&items, query, args...); err != nil {
		logger.Printf("[DATA_STATS] export query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to export data production details"})
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="data-production-statistics.csv"`)

	writer := csv.NewWriter(c.Writer)
	_ = writer.Write([]string{
		"id", "time", "source_id", "source_name", "robot_device_id", "collector_operator_id", "task_id", "task_name",
		"data_type", "status", "count", "duration_ms", "size_bytes", "error_code", "error_message",
	})
	for _, item := range items {
		_ = writer.Write([]string{
			item.ID,
			item.Time,
			item.SourceID,
			item.SourceName,
			item.RobotDeviceID,
			item.CollectorOperatorID,
			item.TaskID,
			item.TaskName,
			item.DataType,
			item.Status,
			strconv.FormatInt(item.Count, 10),
			formatNullableInt64(item.DurationMs),
			formatNullableInt64(item.SizeBytes),
			item.ErrorCode,
			item.ErrorMessage,
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

func (h *DataProductionStatisticsHandler) filteredProductionRecordsSQL(q dataProductionStatsQuery) (string, []interface{}) {
	baseSQL := productionRecordsSQL()
	conditions := []string{"event_time >= ?", "event_time < ?"}
	args := []interface{}{q.StartTime, q.EndTime}

	if q.SourceID != "" {
		conditions = append(conditions, "source_id = ?")
		args = append(args, q.SourceID)
	}
	if q.RobotDeviceID != "" {
		conditions = append(conditions, "robot_device_id = ?")
		args = append(args, q.RobotDeviceID)
	}
	if q.CollectorOperatorID != "" {
		conditions = append(conditions, "collector_operator_id = ?")
		args = append(args, q.CollectorOperatorID)
	}
	if q.DataType != "" {
		conditions = append(conditions, "data_type = ?")
		args = append(args, q.DataType)
	}
	if q.Status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, q.Status)
	}
	if q.TaskID != "" {
		conditions = append(conditions, "task_id = ?")
		args = append(args, q.TaskID)
	}

	query := fmt.Sprintf("SELECT * FROM (%s) production_records WHERE %s", baseSQL, strings.Join(conditions, " AND "))
	return query, args
}

func productionRecordsSQL() string {
	return `
		SELECT
			CONCAT('episode:', e.id) AS id,
			COALESCE(t.completed_at, e.created_at) AS event_time,
			COALESCE(r.device_id, ws.robot_serial, CAST(t.workstation_id AS CHAR), '') AS source_id,
			COALESCE(r.device_id, ws.robot_name, ws.name, CONCAT('workstation:', CAST(t.workstation_id AS CHAR)), 'unknown') AS source_name,
			COALESCE(r.device_id, ws.robot_serial, '') AS robot_device_id,
			COALESCE(dc.operator_id, ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(t.task_id, CAST(e.task_id AS CHAR)) AS task_id,
			COALESCE(t.task_id, CAST(e.task_id AS CHAR)) AS task_name,
			'episode' AS data_type,
			'success' AS status,
			1 AS count_value,
			CAST(COALESCE(e.duration_sec * 1000, TIMESTAMPDIFF(MICROSECOND, t.started_at, t.completed_at) / 1000) AS SIGNED) AS duration_ms,
			e.file_size_bytes AS size_bytes,
			'' AS error_code,
			'' AS error_message
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		WHERE e.deleted_at IS NULL

		UNION ALL

		SELECT
			CONCAT('task:', t.id) AS id,
			COALESCE(t.completed_at, t.updated_at) AS event_time,
			COALESCE(r.device_id, ws.robot_serial, CAST(t.workstation_id AS CHAR), '') AS source_id,
			COALESCE(r.device_id, ws.robot_name, ws.name, CONCAT('workstation:', CAST(t.workstation_id AS CHAR)), 'unknown') AS source_name,
			COALESCE(r.device_id, ws.robot_serial, '') AS robot_device_id,
			COALESCE(dc.operator_id, ws.collector_operator_id, '') AS collector_operator_id,
			t.task_id AS task_id,
			t.task_id AS task_name,
			'episode' AS data_type,
			CASE WHEN t.status = 'cancelled' THEN 'cancelled' ELSE 'failed' END AS status,
			1 AS count_value,
			CASE
				WHEN t.started_at IS NOT NULL AND t.completed_at IS NOT NULL
				THEN CAST(TIMESTAMPDIFF(MICROSECOND, t.started_at, t.completed_at) / 1000 AS SIGNED)
				ELSE NULL
			END AS duration_ms,
			NULL AS size_bytes,
			CASE WHEN t.status = 'cancelled' THEN 'cancelled' ELSE 'failed' END AS error_code,
			COALESCE(t.error_message, '') AS error_message
		FROM tasks t
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		WHERE t.deleted_at IS NULL
		  AND t.status IN ('failed', 'cancelled')
		  AND t.episode_id IS NULL

		UNION ALL

		SELECT
			CONCAT('task:', t.id) AS id,
			COALESCE(t.started_at, t.ready_at, t.updated_at) AS event_time,
			COALESCE(r.device_id, ws.robot_serial, CAST(t.workstation_id AS CHAR), '') AS source_id,
			COALESCE(r.device_id, ws.robot_name, ws.name, CONCAT('workstation:', CAST(t.workstation_id AS CHAR)), 'unknown') AS source_name,
			COALESCE(r.device_id, ws.robot_serial, '') AS robot_device_id,
			COALESCE(dc.operator_id, ws.collector_operator_id, '') AS collector_operator_id,
			t.task_id AS task_id,
			t.task_id AS task_name,
			'episode' AS data_type,
			'processing' AS status,
			1 AS count_value,
			NULL AS duration_ms,
			NULL AS size_bytes,
			'' AS error_code,
			'' AS error_message
		FROM tasks t
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN data_collectors dc ON dc.id = ws.data_collector_id AND dc.deleted_at IS NULL
		WHERE t.deleted_at IS NULL
		  AND t.status IN ('ready', 'in_progress')
		  AND t.episode_id IS NULL
	`
}

func aggregateRowToSummary(row aggregateStatsRow, p95 int64) dataProductionSummaryResponse {
	total := nullInt64(row.TotalCount)
	success := nullInt64(row.SuccessCount)
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

func statsBucketExpression(granularity string) string {
	switch granularity {
	case "hour":
		return "DATE_FORMAT(event_time, '%Y-%m-%dT%H:00:00Z')"
	case "week":
		return "DATE_FORMAT(DATE_SUB(DATE(event_time), INTERVAL WEEKDAY(event_time) DAY), '%Y-%m-%dT00:00:00Z')"
	case "month":
		return "DATE_FORMAT(event_time, '%Y-%m-01T00:00:00Z')"
	default:
		return "DATE_FORMAT(event_time, '%Y-%m-%dT00:00:00Z')"
	}
}

func statsBreakdownExpressions(dimension string) (string, string, error) {
	switch dimension {
	case "source":
		return "source_id", "source_name", nil
	case "robot_device":
		return "robot_device_id", "robot_device_id", nil
	case "collector":
		return "collector_operator_id", "collector_operator_id", nil
	case "task":
		return "task_id", "task_name", nil
	case "data_type":
		return "data_type", "data_type", nil
	default:
		return "", "", fmt.Errorf("dimension must be one of source, robot_device, collector, task, data_type")
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

func formatNullableInt64(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
