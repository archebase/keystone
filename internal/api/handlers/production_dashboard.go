// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
)

const (
	defaultDashboardTrendDays         = 7
	maxDashboardTrendDays             = 31
	defaultDashboardDistributionLimit = 100
	maxDashboardDistributionLimit     = 100
	defaultDashboardActiveLimit       = 20
	maxDashboardActiveLimit           = 100
)

// ProductionDashboardHandler serves aggregate data for production dashboard pages.
type ProductionDashboardHandler struct {
	db *sqlx.DB
}

// NewProductionDashboardHandler creates a production dashboard aggregate handler.
func NewProductionDashboardHandler(db *sqlx.DB) *ProductionDashboardHandler {
	return &ProductionDashboardHandler{db: db}
}

// RegisterRoutes registers production dashboard aggregate routes.
func (h *ProductionDashboardHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/snapshot", h.GetSnapshot)
	apiV1.GET("/batches/:id/task-summary", h.GetBatchTaskSummary)
}

type productionDashboardQuery struct {
	WorkstationID     string
	FactoryID         string
	OrganizationID    string
	TrendDays         int
	DistributionLimit int
	ActiveLimit       int
	TimezoneOffset    string
}

type productionDashboardScope struct {
	Role           string `json:"role"`
	WorkstationID  string `json:"workstation_id,omitempty"`
	FactoryID      string `json:"factory_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	Warning        string `json:"warning,omitempty"`
	collectorID    int64
	empty          bool
}

type productionDashboardSnapshotResponse struct {
	GeneratedAt   string                     `json:"generated_at"`
	Scope         productionDashboardScope   `json:"scope"`
	Tasks         dashboardTaskCounts        `json:"tasks"`
	Trend         []dashboardTrendItem       `json:"trend"`
	Distributions dashboardDistributions     `json:"distributions"`
	Quality       dashboardQuality           `json:"quality"`
	System        dashboardSystemStatus      `json:"system"`
	Stations      []dashboardStationItem     `json:"stations"`
	ActiveBatches []dashboardActiveBatchItem `json:"active_batches"`
	ActiveTasks   []dashboardActiveTaskItem  `json:"active_tasks"`
}

type dashboardTaskCounts struct {
	Total      int64 `json:"total" db:"total"`
	Completed  int64 `json:"completed" db:"completed"`
	InProgress int64 `json:"in_progress" db:"in_progress"`
	Pending    int64 `json:"pending" db:"pending"`
	Ready      int64 `json:"ready" db:"ready"`
	Failed     int64 `json:"failed" db:"failed"`
	Cancelled  int64 `json:"cancelled" db:"cancelled"`
}

type dashboardTrendItem struct {
	Date       string `json:"date" db:"date"`
	Completed  int64  `json:"completed" db:"completed"`
	InProgress int64  `json:"in_progress" db:"in_progress"`
	Pending    int64  `json:"pending" db:"pending"`
}

type dashboardDistributionItem struct {
	ID    string `json:"id" db:"id"`
	Name  string `json:"name" db:"name"`
	Value int64  `json:"value" db:"value"`
}

type dashboardDistributions struct {
	Scene []dashboardDistributionItem `json:"scene"`
	SOP   []dashboardDistributionItem `json:"sop"`
}

type dashboardQuality struct {
	PassRate       float64 `json:"pass_rate"`
	TotalInspected int64   `json:"total_inspected" db:"total_inspected"`
	Passed         int64   `json:"passed" db:"passed"`
	Failed         int64   `json:"failed" db:"failed"`
	Inspecting     int64   `json:"inspecting" db:"inspecting"`
	PendingQA      int64   `json:"pending_qa" db:"pending_qa"`
}

type dashboardQualityRow struct {
	TotalInspected   sql.NullInt64   `db:"total_inspected"`
	Passed           sql.NullInt64   `db:"passed"`
	Failed           sql.NullInt64   `db:"failed"`
	Inspecting       sql.NullInt64   `db:"inspecting"`
	PendingQA        sql.NullInt64   `db:"pending_qa"`
	TotalDurationSec sql.NullFloat64 `db:"total_duration_sec"`
}

type dashboardSystemStatus struct {
	OperatingDays        int64   `json:"operating_days"`
	TotalDataDurationSec float64 `json:"total_data_duration_sec"`
	OnlineStations       int64   `json:"online_stations"`
}

type dashboardStationItem struct {
	ID                  string `json:"id" db:"id"`
	Status              string `json:"status" db:"status"`
	StatusText          string `json:"status_text"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	CollectorName       string `json:"collector_name" db:"collector_name"`
	DeviceID            string `json:"device_id" db:"device_id"`
	DeviceModel         string `json:"device_model" db:"device_model"`
}

type dashboardActiveBatchItem struct {
	ID                  string `json:"id" db:"id"`
	BatchID             string `json:"batch_id" db:"batch_id"`
	OrderID             string `json:"order_id" db:"order_id"`
	OrderName           string `json:"order_name" db:"order_name"`
	SceneName           string `json:"scene_name" db:"scene_name"`
	WorkstationID       string `json:"workstation_id" db:"workstation_id"`
	DeviceID            string `json:"device_id" db:"device_id"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	CollectorName       string `json:"collector_name" db:"collector_name"`
	TaskCount           int64  `json:"task_count" db:"task_count"`
	CompletedCount      int64  `json:"completed_count" db:"completed_count"`
	FailedCount         int64  `json:"failed_count" db:"failed_count"`
	CancelledCount      int64  `json:"cancelled_count" db:"cancelled_count"`
}

type dashboardActiveTaskItem struct {
	ID                  string `json:"id" db:"id"`
	SOPID               string `json:"sop_id" db:"sop_id"`
	SOPLabel            string `json:"sop_label" db:"sop_label"`
	SceneFull           string `json:"scene_full" db:"scene_full"`
	DeviceID            string `json:"device_id" db:"device_id"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	CollectorName       string `json:"collector_name" db:"collector_name"`
}

type dashboardBatchTaskSummaryResponse struct {
	BatchID string                          `json:"batch_id"`
	Items   []dashboardBatchTaskSummaryItem `json:"items"`
}

type dashboardBatchTaskSummaryItem struct {
	SOPID        string           `json:"sop_id" db:"sop_id"`
	SOPLabel     string           `json:"sop_label" db:"sop_label"`
	SceneName    string           `json:"scene_name" db:"scene_name"`
	SubsceneName string           `json:"subscene_name" db:"subscene_name"`
	Total        int64            `json:"total" db:"total"`
	Statuses     map[string]int64 `json:"statuses,omitempty"`
	Pending      int64            `json:"-" db:"pending"`
	Ready        int64            `json:"-" db:"ready"`
	InProgress   int64            `json:"-" db:"in_progress"`
	Completed    int64            `json:"-" db:"completed"`
	Failed       int64            `json:"-" db:"failed"`
	Cancelled    int64            `json:"-" db:"cancelled"`
}

type dashboardDB interface {
	Get(dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
}

// GetSnapshot returns one aggregate snapshot for the production dashboard.
func (h *ProductionDashboardHandler) GetSnapshot(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	q, err := parseProductionDashboardQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	scope, err := h.resolveProductionDashboardScope(c, claims, q)
	if err != nil {
		logger.Printf("[DASHBOARD] scope query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	if scope.empty {
		c.JSON(http.StatusOK, emptyProductionDashboardSnapshot(scope))
		return
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		logger.Printf("[DASHBOARD] begin read transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	tasks, err := h.dashboardTaskCounts(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] task count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	trend, err := h.dashboardTaskTrend(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] task trend query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	sceneDistribution, err := h.dashboardTaskDistribution(tx, scope, "scene", q.DistributionLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] scene distribution query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	sopDistribution, err := h.dashboardTaskDistribution(tx, scope, "sop", q.DistributionLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] sop distribution query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	quality, totalDurationSec, err := h.dashboardQuality(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] quality query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	stations, err := h.dashboardStations(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] stations query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	operatingDays, err := h.dashboardOperatingDays(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] operating days query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	activeBatches, err := h.dashboardActiveBatches(tx, scope, q.ActiveLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] active batches query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}
	activeTasks, err := h.dashboardActiveTasks(tx, scope, q.ActiveLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] active tasks query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[DASHBOARD] commit read transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard"})
		return
	}

	c.JSON(http.StatusOK, productionDashboardSnapshotResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Scope:       scope,
		Tasks:       tasks,
		Trend:       trend,
		Distributions: dashboardDistributions{
			Scene: sceneDistribution,
			SOP:   sopDistribution,
		},
		Quality: quality,
		System: dashboardSystemStatus{
			OperatingDays:        operatingDays,
			TotalDataDurationSec: totalDurationSec,
			OnlineStations:       countOnlineDashboardStations(stations),
		},
		Stations:      stations,
		ActiveBatches: activeBatches,
		ActiveTasks:   activeTasks,
	})
}

// GetBatchTaskSummary returns task status counts grouped by SOP/scene/subscene for one batch.
func (h *ProductionDashboardHandler) GetBatchTaskSummary(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch id"})
		return
	}

	scope, err := h.resolveProductionDashboardScope(c, claims, productionDashboardQuery{})
	if err != nil {
		logger.Printf("[DASHBOARD] scope query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get batch task summary"})
		return
	}
	if scope.empty {
		c.JSON(http.StatusOK, dashboardBatchTaskSummaryResponse{BatchID: strconv.FormatInt(id, 10), Items: []dashboardBatchTaskSummaryItem{}})
		return
	}

	conditions := []string{"b.id = ?", "b.deleted_at IS NULL"}
	args := []interface{}{id}
	conditions, args = appendDashboardBatchScope(conditions, args, scope)

	var exists int
	if err := h.db.Get(&exists, `
		SELECT 1
		FROM batches b
		LEFT JOIN workstations ws ON ws.id = b.workstation_id AND ws.deleted_at IS NULL
		WHERE `+strings.Join(conditions, " AND ")+`
		LIMIT 1
	`, args...); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		logger.Printf("[DASHBOARD] batch scope query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get batch task summary"})
		return
	}

	query := `
		SELECT
			COALESCE(CAST(t.sop_id AS CHAR), '') AS sop_id,
			` + dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version") + ` AS sop_label,
			COALESCE(t.scene_name, '') AS scene_name,
			COALESCE(t.subscene_name, '') AS subscene_name,
			COUNT(1) AS total,
			COALESCE(SUM(CASE WHEN t.status = 'pending' THEN 1 ELSE 0 END), 0) AS pending,
			COALESCE(SUM(CASE WHEN t.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready,
			COALESCE(SUM(CASE WHEN t.status = 'in_progress' THEN 1 ELSE 0 END), 0) AS in_progress,
			COALESCE(SUM(CASE WHEN t.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed,
			COALESCE(SUM(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN t.status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled
		FROM tasks t
		LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL
		WHERE t.batch_id = ? AND t.deleted_at IS NULL
		GROUP BY t.sop_id, t.scene_name, t.subscene_name, s.slug, s.version
		ORDER BY sop_label ASC, scene_name ASC, subscene_name ASC
	`
	items := []dashboardBatchTaskSummaryItem{}
	if err := h.db.Select(&items, query, id); err != nil {
		logger.Printf("[DASHBOARD] batch task summary query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get batch task summary"})
		return
	}
	for i := range items {
		items[i].Statuses = map[string]int64{
			"pending":     items[i].Pending,
			"ready":       items[i].Ready,
			"in_progress": items[i].InProgress,
			"completed":   items[i].Completed,
			"failed":      items[i].Failed,
			"cancelled":   items[i].Cancelled,
		}
	}

	c.JSON(http.StatusOK, dashboardBatchTaskSummaryResponse{
		BatchID: strconv.FormatInt(id, 10),
		Items:   items,
	})
}

func parseProductionDashboardQuery(c *gin.Context) (productionDashboardQuery, error) {
	workstationID, err := parseOptionalPositiveIDQuery(c, "workstation_id")
	if err != nil {
		return productionDashboardQuery{}, err
	}
	factoryID, err := parseOptionalPositiveIDQuery(c, "factory_id")
	if err != nil {
		return productionDashboardQuery{}, err
	}
	orgID, err := parseOptionalPositiveIDQuery(c, "organization_id")
	if err != nil {
		return productionDashboardQuery{}, err
	}
	trendDays, err := parseBoundedIntQuery(c, "trend_days", defaultDashboardTrendDays, 1, maxDashboardTrendDays)
	if err != nil {
		return productionDashboardQuery{}, err
	}
	distributionLimit, err := parseBoundedIntQuery(c, "distribution_limit", defaultDashboardDistributionLimit, 1, maxDashboardDistributionLimit)
	if err != nil {
		return productionDashboardQuery{}, err
	}
	activeLimit, err := parseBoundedIntQuery(c, "active_limit", defaultDashboardActiveLimit, 1, maxDashboardActiveLimit)
	if err != nil {
		return productionDashboardQuery{}, err
	}
	timezoneOffset, err := parseStatsTimezoneOffset(c.Query("timezone_offset"))
	if err != nil {
		return productionDashboardQuery{}, err
	}

	return productionDashboardQuery{
		WorkstationID:     workstationID,
		FactoryID:         factoryID,
		OrganizationID:    orgID,
		TrendDays:         trendDays,
		DistributionLimit: distributionLimit,
		ActiveLimit:       activeLimit,
		TimezoneOffset:    timezoneOffset,
	}, nil
}

func parseOptionalPositiveIDQuery(c *gin.Context, key string) (string, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return "", nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("%s must be a positive integer", key)
	}
	return strconv.FormatInt(id, 10), nil
}

func parseBoundedIntQuery(c *gin.Context, key string, fallback int, minValue int, maxValue int) (int, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	if value > maxValue {
		value = maxValue
	}
	return value, nil
}

func (h *ProductionDashboardHandler) resolveProductionDashboardScope(c *gin.Context, claims *auth.Claims, q productionDashboardQuery) (productionDashboardScope, error) {
	scope := productionDashboardScope{
		Role:           claims.Role,
		WorkstationID:  q.WorkstationID,
		FactoryID:      q.FactoryID,
		OrganizationID: q.OrganizationID,
		collectorID:    claims.CollectorID,
	}

	switch claims.Role {
	case "admin":
		return scope, nil
	case "data_collector":
		var workstationID string
		err := h.db.GetContext(c.Request.Context(), &workstationID, `
			SELECT CAST(id AS CHAR)
			FROM workstations
			WHERE data_collector_id = ? AND deleted_at IS NULL
			LIMIT 1
		`, claims.CollectorID)
		if err == sql.ErrNoRows {
			scope.WorkstationID = ""
			scope.Warning = "workstation not assigned"
			scope.empty = true
			return scope, nil
		}
		if err != nil {
			return productionDashboardScope{}, err
		}
		scope.WorkstationID = workstationID
		scope.FactoryID = ""
		scope.OrganizationID = ""
		return scope, nil
	default:
		return productionDashboardScope{}, fmt.Errorf("unsupported role")
	}
}

func emptyProductionDashboardSnapshot(scope productionDashboardScope) productionDashboardSnapshotResponse {
	return productionDashboardSnapshotResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Scope:       scope,
		Trend:       []dashboardTrendItem{},
		Distributions: dashboardDistributions{
			Scene: []dashboardDistributionItem{},
			SOP:   []dashboardDistributionItem{},
		},
		Stations:      []dashboardStationItem{},
		ActiveBatches: []dashboardActiveBatchItem{},
		ActiveTasks:   []dashboardActiveTaskItem{},
	}
}

func (h *ProductionDashboardHandler) dashboardTaskCounts(db dashboardDB, scope productionDashboardScope) (dashboardTaskCounts, error) {
	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT
			COUNT(1) AS total,
			COALESCE(SUM(CASE WHEN t.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed,
			COALESCE(SUM(CASE WHEN t.status = 'in_progress' THEN 1 ELSE 0 END), 0) AS in_progress,
			COALESCE(SUM(CASE WHEN t.status = 'pending' THEN 1 ELSE 0 END), 0) AS pending,
			COALESCE(SUM(CASE WHEN t.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready,
			COALESCE(SUM(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN t.status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardTaskCounts
	return row, db.Get(&row, query, args...)
}

func (h *ProductionDashboardHandler) dashboardTaskTrend(db dashboardDB, scope productionDashboardScope, q productionDashboardQuery) ([]dashboardTrendItem, error) {
	location := fixedZoneFromOffset(q.TimezoneOffset)
	endLocal := time.Now().In(location).AddDate(0, 0, 1)
	endLocal = time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, location)
	startLocal := endLocal.AddDate(0, 0, -q.TrendDays)
	startUTC := startLocal.UTC()
	endUTC := endLocal.UTC()

	eventTimeExpr := "COALESCE(t.assigned_at, t.started_at, t.completed_at, t.created_at)"
	localEventExpr := "COALESCE(CONVERT_TZ(" + eventTimeExpr + ", @@session.time_zone, ?), " + eventTimeExpr + ")"
	conditions := []string{"t.deleted_at IS NULL", eventTimeExpr + " >= ?", eventTimeExpr + " < ?"}
	args := []interface{}{q.TimezoneOffset, startUTC, endUTC}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)

	query := `
		SELECT
			DATE_FORMAT(` + localEventExpr + `, '%m-%d') AS date,
			COALESCE(SUM(CASE WHEN t.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed,
			COALESCE(SUM(CASE WHEN t.status = 'in_progress' THEN 1 ELSE 0 END), 0) AS in_progress,
			COALESCE(SUM(CASE WHEN t.status IN ('pending', 'ready') THEN 1 ELSE 0 END), 0) AS pending
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ") + `
		GROUP BY date
		ORDER BY MIN(` + eventTimeExpr + `) ASC
	`
	rows := []dashboardTrendItem{}
	if err := db.Select(&rows, query, args...); err != nil {
		return nil, err
	}

	byDate := make(map[string]dashboardTrendItem, len(rows))
	for _, row := range rows {
		byDate[row.Date] = row
	}
	items := make([]dashboardTrendItem, 0, q.TrendDays)
	for day := startLocal; day.Before(endLocal); day = day.AddDate(0, 0, 1) {
		label := day.Format("01-02")
		item, ok := byDate[label]
		if !ok {
			item = dashboardTrendItem{Date: label}
		}
		items = append(items, item)
	}
	return items, nil
}

func (h *ProductionDashboardHandler) dashboardTaskDistribution(db dashboardDB, scope productionDashboardScope, dimension string, limit int) ([]dashboardDistributionItem, error) {
	idExpr := "COALESCE(CAST(t.scene_id AS CHAR), '')"
	nameExpr := "COALESCE(NULLIF(TRIM(t.scene_name), ''), '未分类')"
	joinSQL := ""
	if dimension == "sop" {
		idExpr = "COALESCE(CAST(t.sop_id AS CHAR), '')"
		nameExpr = dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version")
		joinSQL = "LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL"
	}

	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT
			` + idExpr + ` AS id,
			COALESCE(NULLIF(MAX(` + nameExpr + `), ''), '未分类') AS name,
			COUNT(1) AS value
		FROM tasks t
		` + joinSQL + `
		WHERE ` + strings.Join(conditions, " AND ") + `
		GROUP BY ` + idExpr + `
		ORDER BY value DESC, name ASC
		LIMIT ?
	`
	args = append(args, limit)
	items := []dashboardDistributionItem{}
	return items, db.Select(&items, query, args...)
}

func (h *ProductionDashboardHandler) dashboardQuality(db dashboardDB, scope productionDashboardScope) (dashboardQuality, float64, error) {
	conditions := []string{"e.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardEpisodeScope(conditions, args, scope)
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN e.qa_status <> 'pending_qa' THEN 1 ELSE 0 END), 0) AS total_inspected,
			COALESCE(SUM(CASE WHEN e.qa_status IN ('approved', 'inspector_approved') THEN 1 ELSE 0 END), 0) AS passed,
			COALESCE(SUM(CASE WHEN e.qa_status = 'rejected' THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN e.qa_status = 'needs_inspection' THEN 1 ELSE 0 END), 0) AS inspecting,
			COALESCE(SUM(CASE WHEN e.qa_status = 'pending_qa' THEN 1 ELSE 0 END), 0) AS pending_qa,
			COALESCE(SUM(COALESCE(e.duration_sec, 0)), 0) AS total_duration_sec
		FROM episodes e
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardQualityRow
	if err := db.Get(&row, query, args...); err != nil {
		return dashboardQuality{}, 0, err
	}
	inspected := nullInt64(row.TotalInspected)
	passed := nullInt64(row.Passed)
	passRate := 0.0
	if inspected > 0 {
		passRate = math.Round((float64(passed)/float64(inspected))*1000) / 10
	}
	return dashboardQuality{
		PassRate:       passRate,
		TotalInspected: inspected,
		Passed:         passed,
		Failed:         nullInt64(row.Failed),
		Inspecting:     nullInt64(row.Inspecting),
		PendingQA:      nullInt64(row.PendingQA),
	}, nullFloat64(row.TotalDurationSec), nil
}

func (h *ProductionDashboardHandler) dashboardStations(db dashboardDB, scope productionDashboardScope) ([]dashboardStationItem, error) {
	conditions := []string{"ws.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardStationScope(conditions, args, scope)
	query := `
		SELECT
			CAST(ws.id AS CHAR) AS id,
			COALESCE(ws.status, '') AS status,
			COALESCE(ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(ws.robot_serial, '') AS device_id,
			COALESCE(ws.robot_name, '') AS device_model
		FROM workstations ws
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY
			CASE ws.status
				WHEN 'active' THEN 0
				WHEN 'inactive' THEN 1
				WHEN 'break' THEN 2
				WHEN 'offline' THEN 3
				ELSE 9
			END ASC,
			ws.id ASC
	`
	items := []dashboardStationItem{}
	if err := db.Select(&items, query, args...); err != nil {
		return nil, err
	}
	for i := range items {
		items[i].StatusText = dashboardStationStatusText(items[i].Status)
	}
	return items, nil
}

func (h *ProductionDashboardHandler) dashboardOperatingDays(db dashboardDB, scope productionDashboardScope) (int64, error) {
	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT COALESCE(TIMESTAMPDIFF(DAY, MIN(t.created_at), UTC_TIMESTAMP()) + 1, 0)
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ")
	var days sql.NullInt64
	if err := db.Get(&days, query, args...); err != nil {
		return 0, err
	}
	value := nullInt64(days)
	if value < 0 {
		return 0, nil
	}
	return value, nil
}

func (h *ProductionDashboardHandler) dashboardActiveBatches(db dashboardDB, scope productionDashboardScope, limit int) ([]dashboardActiveBatchItem, error) {
	query, args := buildDashboardActiveBatchesQuery(scope, limit)
	items := []dashboardActiveBatchItem{}
	return items, db.Select(&items, query, args...)
}

func buildDashboardActiveBatchesQuery(scope productionDashboardScope, limit int) (string, []interface{}) {
	conditions := []string{"b.deleted_at IS NULL", "b.status = 'active'"}
	args := []interface{}{}
	conditions, args = appendDashboardBatchScope(conditions, args, scope)
	query := `
		WITH active_batches AS (
			SELECT
				b.id,
				b.batch_id,
				b.order_id,
				b.workstation_id,
				b.started_at,
				b.updated_at,
				b.created_at
			FROM batches b
			LEFT JOIN workstations ws ON ws.id = b.workstation_id AND ws.deleted_at IS NULL
			WHERE ` + strings.Join(conditions, " AND ") + `
			ORDER BY COALESCE(b.started_at, b.updated_at, b.created_at) DESC, b.id DESC
			LIMIT ?
		)
		SELECT
			CAST(b.id AS CHAR) AS id,
			COALESCE(b.batch_id, '') AS batch_id,
			COALESCE(CAST(b.order_id AS CHAR), '') AS order_id,
			COALESCE(o.name, '') AS order_name,
			COALESCE(sc.name, '') AS scene_name,
			COALESCE(CAST(b.workstation_id AS CHAR), '') AS workstation_id,
			COALESCE(ws.robot_serial, '') AS device_id,
			COALESCE(ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(tc.task_count, 0) AS task_count,
			COALESCE(tc.completed_count, 0) AS completed_count,
			COALESCE(tc.failed_count, 0) AS failed_count,
			COALESCE(tc.cancelled_count, 0) AS cancelled_count
		FROM active_batches b
		LEFT JOIN orders o ON o.id = b.order_id AND o.deleted_at IS NULL
		LEFT JOIN scenes sc ON sc.id = o.scene_id AND sc.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = b.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN (
			SELECT
				ab.id AS batch_id,
				COUNT(t.id) AS task_count,
				COALESCE(SUM(CASE WHEN t.status = 'completed' THEN 1 ELSE 0 END), 0) AS completed_count,
				COALESCE(SUM(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed_count,
				COALESCE(SUM(CASE WHEN t.status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled_count
			FROM active_batches ab
			LEFT JOIN tasks t ON t.batch_id = ab.id AND t.deleted_at IS NULL
			GROUP BY ab.id
		) tc ON tc.batch_id = b.id
		ORDER BY COALESCE(b.started_at, b.updated_at, b.created_at) DESC, b.id DESC
	`
	args = append(args, limit)
	return query, args
}

func (h *ProductionDashboardHandler) dashboardActiveTasks(db dashboardDB, scope productionDashboardScope, limit int) ([]dashboardActiveTaskItem, error) {
	conditions := []string{"t.deleted_at IS NULL", "t.status = 'in_progress'"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT
			COALESCE(t.task_id, CAST(t.id AS CHAR)) AS id,
			COALESCE(CAST(t.sop_id AS CHAR), '') AS sop_id,
			` + dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version") + ` AS sop_label,
			CASE
				WHEN NULLIF(TRIM(COALESCE(t.scene_name, '')), '') IS NOT NULL
					AND NULLIF(TRIM(COALESCE(t.subscene_name, '')), '') IS NOT NULL
					THEN CONCAT(t.scene_name, '@', t.subscene_name)
				ELSE COALESCE(NULLIF(TRIM(t.scene_name), ''), NULLIF(TRIM(t.subscene_name), ''), '-')
			END AS scene_full,
			COALESCE(ws.robot_serial, '') AS device_id,
			COALESCE(ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(ws.collector_name, '') AS collector_name
		FROM tasks t
		LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY COALESCE(t.started_at, t.assigned_at, t.updated_at, t.created_at) DESC, t.id DESC
		LIMIT ?
	`
	args = append(args, limit)
	items := []dashboardActiveTaskItem{}
	return items, db.Select(&items, query, args...)
}

func appendDashboardTaskScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.id = t.workstation_id
				AND ws_scope.data_collector_id = ?
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(t.workstation_id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.FactoryID != "" {
		conditions = append(conditions, "CAST(t.factory_id AS CHAR) = ?")
		args = append(args, scope.FactoryID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(t.organization_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func appendDashboardEpisodeScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.id = e.workstation_id
				AND ws_scope.data_collector_id = ?
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(e.workstation_id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.FactoryID != "" {
		conditions = append(conditions, "CAST(e.factory_id AS CHAR) = ?")
		args = append(args, scope.FactoryID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(e.organization_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func appendDashboardStationScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		conditions = append(conditions, "ws.data_collector_id = ?")
		args = append(args, scope.collectorID)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(ws.id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.FactoryID != "" {
		conditions = append(conditions, "CAST(ws.factory_id AS CHAR) = ?")
		args = append(args, scope.FactoryID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(ws.organization_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func appendDashboardBatchScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		conditions = append(conditions, "ws.data_collector_id = ?")
		args = append(args, scope.collectorID)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(b.workstation_id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.FactoryID != "" {
		conditions = append(conditions, "CAST(ws.factory_id AS CHAR) = ?")
		args = append(args, scope.FactoryID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(b.organization_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func dashboardSOPLabelSQL(sopIDExpr string, slugExpr string, versionExpr string) string {
	return `CASE
		WHEN ` + sopIDExpr + ` IS NULL THEN '未分类'
		WHEN NULLIF(` + slugExpr + `, '') IS NULL THEN CONCAT('SOP #', CAST(` + sopIDExpr + ` AS CHAR))
		WHEN NULLIF(` + versionExpr + `, '') IS NULL THEN ` + slugExpr + `
		ELSE CONCAT(` + slugExpr + `, '@', ` + versionExpr + `)
	END`
}

func dashboardStationStatusText(status string) string {
	switch status {
	case "active":
		return "在线"
	case "inactive":
		return "待命中"
	case "break":
		return "休息中"
	case "offline":
		return "离线"
	default:
		return status
	}
}

func countOnlineDashboardStations(stations []dashboardStationItem) int64 {
	var count int64
	for _, station := range stations {
		if station.Status == "active" {
			count++
		}
	}
	return count
}

func fixedZoneFromOffset(offset string) *time.Location {
	if len(offset) != 6 {
		return time.UTC
	}
	sign := 1
	if offset[0] == '-' {
		sign = -1
	}
	hours, _ := strconv.Atoi(offset[1:3])
	minutes, _ := strconv.Atoi(offset[4:6])
	return time.FixedZone(offset, sign*((hours*60+minutes)*60))
}

func nullFloat64(v sql.NullFloat64) float64 {
	if !v.Valid {
		return 0
	}
	return v.Float64
}
