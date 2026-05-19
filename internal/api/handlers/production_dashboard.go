// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
)

const (
	defaultDashboardTrendDays         = 7
	maxDashboardTrendDays             = 31
	defaultDashboardDistributionLimit = 100
	maxDashboardDistributionLimit     = 100
	defaultDashboardActiveLimit       = 20
	maxDashboardActiveLimit           = 100
	defaultDashboardRecentLimit       = 10
	maxDashboardRecentLimit           = 50
	defaultDashboardPreviewLimit      = 8
	maxDashboardPreviewLimit          = 20
)

// ProductionDashboardHandler serves aggregate data for production dashboard pages.
type ProductionDashboardHandler struct {
	db          *sqlx.DB
	recorderHub *services.RecorderHub
	transferHub *services.TransferHub
}

// NewProductionDashboardHandler creates a production dashboard aggregate handler.
func NewProductionDashboardHandler(db *sqlx.DB, recorderHub *services.RecorderHub, transferHub *services.TransferHub) *ProductionDashboardHandler {
	return &ProductionDashboardHandler{
		db:          db,
		recorderHub: recorderHub,
		transferHub: transferHub,
	}
}

// RegisterRoutes registers production dashboard aggregate routes.
func (h *ProductionDashboardHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/snapshot", h.GetSnapshot)
	apiV1.GET("/overview", h.GetOverview)
	apiV1.GET("/batches/:id/task-summary", h.GetBatchTaskSummary)
}

type productionDashboardQuery struct {
	WorkstationID     string
	FactoryID         string
	OrganizationID    string
	TrendDays         int
	DistributionLimit int
	ActiveLimit       int
	RecentLimit       int
	PreviewLimit      int
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

type productionDashboardOverviewResponse struct {
	GeneratedAt            string                            `json:"generated_at"`
	Scope                  productionDashboardScope          `json:"scope"`
	Summary                dashboardOverviewSummary          `json:"summary"`
	Trend                  []dashboardTrendItem              `json:"trend"`
	TaskStatusDistribution []dashboardStatusDistributionItem `json:"task_status_distribution"`
	Quality                dashboardOverviewQuality          `json:"quality"`
	Devices                dashboardOverviewDevices          `json:"devices"`
	Stations               dashboardOverviewStations         `json:"stations"`
	RecentTasks            []dashboardRecentTaskItem         `json:"recent_tasks"`
	Previews               []dashboardPreviewItem            `json:"previews"`
}

type dashboardOverviewSummary struct {
	TotalTasks           int64   `json:"total_tasks"`
	TodayTasks           int64   `json:"today_tasks"`
	CompletedTasks       int64   `json:"completed_tasks"`
	InProgressTasks      int64   `json:"in_progress_tasks"`
	PendingTasks         int64   `json:"pending_tasks"`
	FailedTasks          int64   `json:"failed_tasks"`
	CancelledTasks       int64   `json:"cancelled_tasks"`
	ActiveBatches        int64   `json:"active_batches"`
	ActiveStations       int64   `json:"active_stations"`
	TotalStations        int64   `json:"total_stations"`
	RobotOnlineRate      float64 `json:"robot_online_rate"`
	QualityPassRate      float64 `json:"quality_pass_rate"`
	TotalDataDurationSec float64 `json:"total_data_duration_sec"`
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
	Total      int64  `json:"total" db:"total"`
	Completed  int64  `json:"completed" db:"completed"`
	InProgress int64  `json:"in_progress" db:"in_progress"`
	Pending    int64  `json:"pending" db:"pending"`
	Failed     int64  `json:"failed" db:"failed"`
}

type dashboardStatusDistributionItem struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Value  int64  `json:"value"`
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

type dashboardOverviewQuality struct {
	PassRate       float64                         `json:"pass_rate"`
	TotalInspected int64                           `json:"total_inspected"`
	Passed         int64                           `json:"passed"`
	Failed         int64                           `json:"failed"`
	Inspecting     int64                           `json:"inspecting"`
	PendingQA      int64                           `json:"pending_qa"`
	RecentFailures []dashboardQualityRecentFailure `json:"recent_failures"`
}

type dashboardQualityRecentFailure struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
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

type dashboardOverviewDevices struct {
	Summary dashboardDeviceSummary `json:"summary"`
}

type dashboardDeviceSummary struct {
	Total      int64   `json:"total"`
	Online     int64   `json:"online"`
	Offline    int64   `json:"offline"`
	OnlineRate float64 `json:"online_rate"`
}

type dashboardOverviewStations struct {
	Summary dashboardStationSummary `json:"summary"`
}

type dashboardStationSummary struct {
	Total      int64   `json:"total"`
	Online     int64   `json:"online"`
	Active     int64   `json:"active"`
	Inactive   int64   `json:"inactive"`
	Break      int64   `json:"break"`
	Offline    int64   `json:"offline"`
	OnlineRate float64 `json:"online_rate"`
}

type dashboardDeviceConnectionRow struct {
	DeviceID string `db:"device_id"`
}

type dashboardStationItem struct {
	ID                  string `json:"id" db:"id"`
	Name                string `json:"name" db:"name"`
	Status              string `json:"status" db:"status"`
	StatusText          string `json:"status_text"`
	CollectorOperatorID string `json:"collector_operator_id" db:"collector_operator_id"`
	CollectorName       string `json:"collector_name" db:"collector_name"`
	DeviceID            string `json:"device_id" db:"device_id"`
	DeviceModel         string `json:"device_model" db:"device_model"`
	RobotName           string `json:"robot_name" db:"robot_name"`
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

type dashboardRecentTaskItem struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	TaskName    string `json:"task_name"`
	Status      string `json:"status"`
	StatusText  string `json:"status_text"`
	RobotName   string `json:"robot_name"`
	StationName string `json:"station_name"`
	BatchID     string `json:"batch_id"`
	SceneName   string `json:"scene_name"`
	SOPLabel    string `json:"sop_label"`
	EpisodeID   string `json:"episode_id"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type dashboardRecentTaskRow struct {
	ID          string       `db:"id"`
	TaskID      string       `db:"task_id"`
	TaskName    string       `db:"task_name"`
	Status      string       `db:"status"`
	RobotName   string       `db:"robot_name"`
	StationName string       `db:"station_name"`
	BatchID     string       `db:"batch_id"`
	SceneName   string       `db:"scene_name"`
	SOPLabel    string       `db:"sop_label"`
	EpisodeID   string       `db:"episode_id"`
	CreatedAt   sql.NullTime `db:"created_at"`
	UpdatedAt   sql.NullTime `db:"updated_at"`
}

type dashboardPreviewItem struct {
	ID              string  `json:"id"`
	SceneName       string  `json:"scene_name"`
	SOPLabel        string  `json:"sop_label"`
	DeviceType      string  `json:"device_type"`
	DeviceID        string  `json:"device_id"`
	StationName     string  `json:"station_name"`
	Status          string  `json:"status"`
	VideoURL        string  `json:"video_url"`
	PreviewURL      string  `json:"preview_url"`
	PosterURL       string  `json:"poster_url"`
	DurationSeconds float64 `json:"duration_seconds"`
	CreatedAt       string  `json:"created_at"`
	EpisodeID       string  `json:"episode_id"`
	TaskID          string  `json:"task_id"`
}

type dashboardPreviewRow struct {
	ID              string          `db:"id"`
	SceneName       string          `db:"scene_name"`
	SOPLabel        string          `db:"sop_label"`
	DeviceType      string          `db:"device_type"`
	DeviceID        string          `db:"device_id"`
	StationName     string          `db:"station_name"`
	Status          string          `db:"status"`
	McapPath        string          `db:"mcap_path"`
	DurationSeconds sql.NullFloat64 `db:"duration_seconds"`
	CreatedAt       sql.NullTime    `db:"created_at"`
	EpisodeID       string          `db:"episode_id"`
	TaskID          string          `db:"task_id"`
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

// GetOverview returns the production big-screen aggregate contract.
//
// @Summary      Get production dashboard overview
// @Description  Returns one aggregate overview for the Synapse production big screen.
// @Tags         production-dashboard
// @Accept       json
// @Produce      json
// @Param        workstation_id query int false "Filter by workstation ID; ignored for data_collector"
// @Param        factory_id query int false "Filter by factory ID; ignored for data_collector"
// @Param        organization_id query int false "Filter by organization ID; ignored for data_collector"
// @Param        timezone_offset query string false "Timezone offset such as +08:00"
// @Param        trend_days query int false "Trend day count (default 7, max 31)"
// @Param        active_limit query int false "Active batch limit (default 20, max 100)"
// @Param        recent_limit query int false "Recent task limit (default 10, max 50)"
// @Param        preview_limit query int false "Preview limit (default 8, max 20)"
// @Success      200 {object} map[string]interface{} "generated_at, scope, summary, trend, task_status_distribution, quality, devices, stations, recent_tasks, previews"
// @Failure      400 {object} map[string]string
// @Failure      401 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /production/dashboard/overview [get]
func (h *ProductionDashboardHandler) GetOverview(c *gin.Context) {
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
		logger.Printf("[DASHBOARD] overview scope query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	if scope.empty {
		c.JSON(http.StatusOK, emptyProductionDashboardOverview(scope))
		return
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		logger.Printf("[DASHBOARD] overview begin read transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	tasks, err := h.dashboardTaskCounts(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] overview task count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	todayTasks, err := h.dashboardTodayTaskCount(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] overview today task query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	trend, err := h.dashboardDataProductionTrend(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] overview data production trend query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	quality, totalDurationSec, err := h.dashboardQuality(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] overview quality query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	stations, err := h.dashboardStations(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] overview station query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	deviceConnections, err := h.dashboardDeviceConnections(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] overview device connection query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	activeBatches, err := h.dashboardActiveBatches(tx, scope, q.ActiveLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] overview active batch query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	recentTasks, err := h.dashboardRecentTasks(tx, scope, q.RecentLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] overview recent task query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	previews, err := h.dashboardPreviews(tx, scope, q.PreviewLimit)
	if err != nil {
		logger.Printf("[DASHBOARD] overview preview query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[DASHBOARD] overview commit read transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}

	devices := buildOverviewDevices(deviceConnections, func(deviceID string) bool {
		return services.IsRobotConnected(h.recorderHub, h.transferHub, deviceID)
	})
	stationsOverview := buildOverviewStations(stations)
	c.JSON(http.StatusOK, productionDashboardOverviewResponse{
		GeneratedAt:            time.Now().UTC().Format(time.RFC3339),
		Scope:                  scope,
		Summary:                buildOverviewSummary(tasks, todayTasks, quality, totalDurationSec, devices, stationsOverview, len(activeBatches)),
		Trend:                  trend,
		TaskStatusDistribution: buildTaskStatusDistribution(tasks),
		Quality: dashboardOverviewQuality{
			PassRate:       quality.PassRate,
			TotalInspected: quality.TotalInspected,
			Passed:         quality.Passed,
			Failed:         quality.Failed,
			Inspecting:     quality.Inspecting,
			PendingQA:      quality.PendingQA,
			RecentFailures: []dashboardQualityRecentFailure{},
		},
		Devices:     devices,
		Stations:    stationsOverview,
		RecentTasks: recentTasks,
		Previews:    previews,
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
	recentLimit, err := parseBoundedIntQuery(c, "recent_limit", defaultDashboardRecentLimit, 1, maxDashboardRecentLimit)
	if err != nil {
		return productionDashboardQuery{}, err
	}
	previewLimit, err := parseBoundedIntQuery(c, "preview_limit", defaultDashboardPreviewLimit, 1, maxDashboardPreviewLimit)
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
		RecentLimit:       recentLimit,
		PreviewLimit:      previewLimit,
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
	case "admin", "display":
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

func emptyProductionDashboardOverview(scope productionDashboardScope) productionDashboardOverviewResponse {
	return productionDashboardOverviewResponse{
		GeneratedAt:            time.Now().UTC().Format(time.RFC3339),
		Scope:                  scope,
		Trend:                  []dashboardTrendItem{},
		TaskStatusDistribution: []dashboardStatusDistributionItem{},
		Quality: dashboardOverviewQuality{
			RecentFailures: []dashboardQualityRecentFailure{},
		},
		Devices:     dashboardOverviewDevices{},
		Stations:    dashboardOverviewStations{},
		RecentTasks: []dashboardRecentTaskItem{},
		Previews:    []dashboardPreviewItem{},
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

func (h *ProductionDashboardHandler) dashboardTodayTaskCount(db dashboardDB, scope productionDashboardScope, q productionDashboardQuery) (int64, error) {
	location := fixedZoneFromOffset(q.TimezoneOffset)
	nowLocal := time.Now().In(location)
	startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, location)
	endLocal := startLocal.AddDate(0, 0, 1)

	conditions := []string{"t.deleted_at IS NULL", "t.created_at >= ?", "t.created_at < ?"}
	args := []interface{}{startLocal.UTC(), endLocal.UTC()}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT COUNT(1)
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ")

	var count sql.NullInt64
	if err := db.Get(&count, query, args...); err != nil {
		return 0, err
	}
	return nullInt64(count), nil
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
			COALESCE(SUM(CASE WHEN t.status IN ('pending', 'ready') THEN 1 ELSE 0 END), 0) AS pending,
			COALESCE(SUM(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed
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
		row.Total = row.Completed + row.InProgress + row.Pending + row.Failed
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

func (h *ProductionDashboardHandler) dashboardDataProductionTrend(db dashboardDB, scope productionDashboardScope, q productionDashboardQuery) ([]dashboardTrendItem, error) {
	location := fixedZoneFromOffset(q.TimezoneOffset)
	endLocal := time.Now().In(location).AddDate(0, 0, 1)
	endLocal = time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), 0, 0, 0, 0, location)
	startLocal := endLocal.AddDate(0, 0, -q.TrendDays)
	startUTC := startLocal.UTC()
	endUTC := endLocal.UTC()

	eventTimeExpr := "COALESCE(t.completed_at, e.created_at)"
	localEventExpr := "COALESCE(CONVERT_TZ(" + eventTimeExpr + ", @@session.time_zone, ?), " + eventTimeExpr + ")"
	conditions := []string{"e.deleted_at IS NULL", eventTimeExpr + " >= ?", eventTimeExpr + " < ?"}
	args := []interface{}{q.TimezoneOffset, startUTC, endUTC}
	conditions, args = appendDashboardEpisodeScope(conditions, args, scope)

	query := `
		SELECT
			DATE_FORMAT(` + localEventExpr + `, '%m-%d') AS date,
			COUNT(e.id) AS total
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
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
			COALESCE(ws.name, '') AS name,
			COALESCE(ws.status, '') AS status,
			COALESCE(ws.collector_operator_id, '') AS collector_operator_id,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(ws.robot_serial, '') AS device_id,
			COALESCE(ws.robot_name, '') AS device_model,
			COALESCE(ws.robot_name, '') AS robot_name
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

func (h *ProductionDashboardHandler) dashboardDeviceConnections(db dashboardDB, scope productionDashboardScope) ([]dashboardDeviceConnectionRow, error) {
	conditions := []string{"r.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardRobotScope(conditions, args, scope)
	query := `
		SELECT COALESCE(NULLIF(TRIM(r.device_id), ''), '') AS device_id
		FROM robots r
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY r.device_id ASC
	`
	items := []dashboardDeviceConnectionRow{}
	return items, db.Select(&items, query, args...)
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

func (h *ProductionDashboardHandler) dashboardRecentTasks(db dashboardDB, scope productionDashboardScope, limit int) ([]dashboardRecentTaskItem, error) {
	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	taskNameExpr := dashboardTaskNameSQL("t.scene_name", "t.subscene_name", "t.sop_id", "s.slug", "s.version")
	updatedAtExpr := dashboardRecentTaskUpdatedAtSQL("t")
	query := `
		SELECT
			CAST(t.id AS CHAR) AS id,
			COALESCE(NULLIF(t.task_id, ''), CAST(t.id AS CHAR)) AS task_id,
			` + taskNameExpr + ` AS task_name,
			COALESCE(t.status, '') AS status,
			COALESCE(ws.robot_name, ws.robot_serial, '') AS robot_name,
			COALESCE(ws.name, CAST(ws.id AS CHAR), '') AS station_name,
			COALESCE(b.batch_id, '') AS batch_id,
			COALESCE(NULLIF(t.scene_name, ''), '') AS scene_name,
			` + dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version") + ` AS sop_label,
			COALESCE(e.episode_id, '') AS episode_id,
			t.created_at AS created_at,
			` + updatedAtExpr + ` AS updated_at
		FROM tasks t
		LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL
		LEFT JOIN batches b ON b.id = t.batch_id AND b.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN (
			SELECT task_id, MAX(id) AS latest_id
			FROM episodes
			WHERE deleted_at IS NULL
			GROUP BY task_id
		) latest_episode ON latest_episode.task_id = t.id
		LEFT JOIN episodes e ON e.id = latest_episode.latest_id AND e.deleted_at IS NULL
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY
			` + updatedAtExpr + ` DESC,
			t.id DESC
		LIMIT ?
	`
	args = append(args, limit)
	rows := []dashboardRecentTaskRow{}
	if err := db.Select(&rows, query, args...); err != nil {
		return nil, err
	}

	items := make([]dashboardRecentTaskItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, dashboardRecentTaskItem{
			ID:          row.ID,
			TaskID:      row.TaskID,
			TaskName:    row.TaskName,
			Status:      row.Status,
			StatusText:  dashboardTaskStatusText(row.Status),
			RobotName:   row.RobotName,
			StationName: row.StationName,
			BatchID:     row.BatchID,
			SceneName:   row.SceneName,
			SOPLabel:    row.SOPLabel,
			EpisodeID:   row.EpisodeID,
			CreatedAt:   formatNullableTime(row.CreatedAt),
			UpdatedAt:   formatNullableTime(row.UpdatedAt),
		})
	}
	return items, nil
}

func dashboardRecentTaskUpdatedAtSQL(taskAlias string) string {
	prefix := strings.TrimSpace(taskAlias)
	if prefix != "" {
		prefix += "."
	}
	return `COALESCE(` + prefix + `updated_at, ` + prefix + `completed_at, ` + prefix + `started_at, ` + prefix + `assigned_at, ` + prefix + `created_at)`
}

func (h *ProductionDashboardHandler) dashboardPreviews(db dashboardDB, scope productionDashboardScope, limit int) ([]dashboardPreviewItem, error) {
	conditions := []string{"e.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardEpisodeScope(conditions, args, scope)
	sceneNameExpr := dashboardTaskNameSQL("t.scene_name", "t.subscene_name", "t.sop_id", "s.slug", "s.version")
	query := `
		SELECT
			CAST(e.id AS CHAR) AS id,
			` + sceneNameExpr + ` AS scene_name,
			` + dashboardSOPLabelSQL("t.sop_id", "s.slug", "s.version") + ` AS sop_label,
			COALESCE(NULLIF(rt.name, ''), NULLIF(rt.model, ''), NULLIF(ws.robot_name, ''), '') AS device_type,
			COALESCE(ws.robot_serial, r.device_id, '') AS device_id,
			COALESCE(ws.name, CAST(ws.id AS CHAR), '') AS station_name,
			COALESCE(NULLIF(t.status, ''), e.qa_status, '') AS status,
			COALESCE(e.mcap_path, '') AS mcap_path,
			e.duration_sec AS duration_seconds,
			e.created_at AS created_at,
			COALESCE(e.episode_id, '') AS episode_id,
			COALESCE(NULLIF(t.task_id, ''), CAST(t.id AS CHAR), '') AS task_id
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT ?
	`
	args = append(args, limit)
	rows := []dashboardPreviewRow{}
	if err := db.Select(&rows, query, args...); err != nil {
		return nil, err
	}

	items := make([]dashboardPreviewItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, dashboardPreviewItem{
			ID:              row.ID,
			SceneName:       row.SceneName,
			SOPLabel:        row.SOPLabel,
			DeviceType:      row.DeviceType,
			DeviceID:        row.DeviceID,
			StationName:     row.StationName,
			Status:          row.Status,
			VideoURL:        "",
			PreviewURL:      dashboardEpisodePreviewURL(row.ID, row.McapPath),
			PosterURL:       "",
			DurationSeconds: nullFloat64(row.DurationSeconds),
			CreatedAt:       formatNullableTime(row.CreatedAt),
			EpisodeID:       row.EpisodeID,
			TaskID:          row.TaskID,
		})
	}
	return items, nil
}

func dashboardEpisodePreviewURL(id string, mcapPath string) string {
	episodeID := strings.TrimSpace(id)
	if episodeID == "" || strings.TrimSpace(mcapPath) == "" {
		return ""
	}
	return "/api/v1/episodes/" + url.PathEscape(episodeID) + "/presign?kind=mcap"
}

func buildOverviewSummary(
	tasks dashboardTaskCounts,
	todayTasks int64,
	quality dashboardQuality,
	totalDurationSec float64,
	devices dashboardOverviewDevices,
	stations dashboardOverviewStations,
	activeBatchCount int,
) dashboardOverviewSummary {
	return dashboardOverviewSummary{
		TotalTasks:           tasks.Total,
		TodayTasks:           todayTasks,
		CompletedTasks:       tasks.Completed,
		InProgressTasks:      tasks.InProgress,
		PendingTasks:         tasks.Pending + tasks.Ready,
		FailedTasks:          tasks.Failed,
		CancelledTasks:       tasks.Cancelled,
		ActiveBatches:        int64(activeBatchCount),
		ActiveStations:       stations.Summary.Active,
		TotalStations:        stations.Summary.Total,
		RobotOnlineRate:      devices.Summary.OnlineRate,
		QualityPassRate:      quality.PassRate,
		TotalDataDurationSec: totalDurationSec,
	}
}

func buildTaskStatusDistribution(tasks dashboardTaskCounts) []dashboardStatusDistributionItem {
	items := []dashboardStatusDistributionItem{
		{Status: "pending", Label: dashboardTaskStatusText("pending"), Value: tasks.Pending},
		{Status: "ready", Label: dashboardTaskStatusText("ready"), Value: tasks.Ready},
		{Status: "in_progress", Label: dashboardTaskStatusText("in_progress"), Value: tasks.InProgress},
		{Status: "completed", Label: dashboardTaskStatusText("completed"), Value: tasks.Completed},
		{Status: "failed", Label: dashboardTaskStatusText("failed"), Value: tasks.Failed},
		{Status: "cancelled", Label: dashboardTaskStatusText("cancelled"), Value: tasks.Cancelled},
	}
	filtered := make([]dashboardStatusDistributionItem, 0, len(items))
	for _, item := range items {
		if item.Value > 0 {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func buildOverviewDevices(devices []dashboardDeviceConnectionRow, connected func(string) bool) dashboardOverviewDevices {
	var summary dashboardDeviceSummary
	for _, device := range devices {
		summary.Total++
		if connected != nil && connected(device.DeviceID) {
			summary.Online++
			continue
		}
		summary.Offline++
	}
	if summary.Total > 0 {
		summary.OnlineRate = math.Round((float64(summary.Online)/float64(summary.Total))*1000) / 10
	}
	return dashboardOverviewDevices{
		Summary: summary,
	}
}

func buildOverviewStations(stations []dashboardStationItem) dashboardOverviewStations {
	var summary dashboardStationSummary
	for _, station := range stations {
		summary.Total++
		switch station.Status {
		case "active":
			summary.Active++
			summary.Online++
		case "inactive":
			summary.Inactive++
			summary.Online++
		case "break":
			summary.Break++
			summary.Online++
		default:
			summary.Offline++
		}
	}
	if summary.Total > 0 {
		summary.OnlineRate = math.Round((float64(summary.Online)/float64(summary.Total))*1000) / 10
	}
	return dashboardOverviewStations{Summary: summary}
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

func appendDashboardRobotScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.robot_id = r.id
				AND ws_scope.data_collector_id = ?
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.robot_id = r.id
				AND CAST(ws_scope.id AS CHAR) = ?
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.WorkstationID)
	}
	if scope.FactoryID != "" {
		conditions = append(conditions, "CAST(r.factory_id AS CHAR) = ?")
		args = append(args, scope.FactoryID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.robot_id = r.id
				AND CAST(ws_scope.organization_id AS CHAR) = ?
				AND ws_scope.deleted_at IS NULL
		)`)
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

func dashboardTaskNameSQL(sceneExpr string, subsceneExpr string, sopIDExpr string, slugExpr string, versionExpr string) string {
	return `CASE
		WHEN NULLIF(TRIM(COALESCE(` + sceneExpr + `, '')), '') IS NOT NULL
			AND NULLIF(TRIM(COALESCE(` + subsceneExpr + `, '')), '') IS NOT NULL
			THEN CONCAT(` + sceneExpr + `, ' / ', ` + subsceneExpr + `)
		WHEN NULLIF(TRIM(COALESCE(` + sceneExpr + `, '')), '') IS NOT NULL THEN ` + sceneExpr + `
		WHEN NULLIF(TRIM(COALESCE(` + subsceneExpr + `, '')), '') IS NOT NULL THEN ` + subsceneExpr + `
		ELSE ` + dashboardSOPLabelSQL(sopIDExpr, slugExpr, versionExpr) + `
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

func dashboardTaskStatusText(status string) string {
	switch status {
	case "pending":
		return "待开始"
	case "ready":
		return "就绪"
	case "in_progress":
		return "进行中"
	case "completed":
		return "已完成"
	case "failed":
		return "失败"
	case "cancelled":
		return "已取消"
	default:
		return firstNonEmpty(status, "未知")
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

func formatNullableTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nullFloat64(v sql.NullFloat64) float64 {
	if !v.Valid {
		return 0
	}
	return v.Float64
}
