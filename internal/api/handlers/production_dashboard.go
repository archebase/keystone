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
	defaultDashboardTrendDays    = 7
	maxDashboardTrendDays        = 31
	defaultDashboardRecentLimit  = 10
	maxDashboardRecentLimit      = 50
	defaultDashboardPreviewLimit = 8
	maxDashboardPreviewLimit     = 20
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
	apiV1.GET("/overview", h.GetOverview)
}

type productionDashboardQuery struct {
	WorkstationID  string
	OrganizationID string
	TrendDays      int
	RecentLimit    int
	PreviewLimit   int
	TimezoneOffset string
}

type productionDashboardScope struct {
	Role           string `json:"role"`
	WorkstationID  string `json:"workstation_id,omitempty"`
	OrganizationID string `json:"organization_id,omitempty"`
	Warning        string `json:"warning,omitempty"`
	collectorID    int64
	workspaceIDs   []int64
	empty          bool
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
	TotalEpisodes        int64   `json:"total_episodes"`
	TodayTasks           int64   `json:"today_tasks"`
	TodayEpisodes        int64   `json:"today_episodes"`
	CompletedTasks       int64   `json:"completed_tasks"`
	InProgressTasks      int64   `json:"in_progress_tasks"`
	UploadingTasks       int64   `json:"uploading_tasks"`
	PendingTasks         int64   `json:"pending_tasks"`
	FailedTasks          int64   `json:"failed_tasks"`
	CancelledTasks       int64   `json:"cancelled_tasks"`
	ActivePlans          int64   `json:"active_plans"`
	ActiveStations       int64   `json:"active_stations"`
	TotalStations        int64   `json:"total_stations"`
	RobotOnlineRate      float64 `json:"robot_online_rate"`
	QualityPassRate      float64 `json:"quality_pass_rate"`
	TotalDataDurationSec float64 `json:"total_data_duration_sec"`
	TotalDataSizeBytes   int64   `json:"total_data_size_bytes"`
	TodayDataDurationSec float64 `json:"today_data_duration_sec"`
	TodayDataSizeBytes   int64   `json:"today_data_size_bytes"`
}

type dashboardTaskCounts struct {
	Total      int64 `json:"total" db:"total"`
	Completed  int64 `json:"completed" db:"completed"`
	InProgress int64 `json:"in_progress" db:"in_progress"`
	Uploading  int64 `json:"uploading" db:"uploading"`
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
	Uploading  int64  `json:"uploading" db:"uploading"`
	Pending    int64  `json:"pending" db:"pending"`
	Failed     int64  `json:"failed" db:"failed"`
}

type dashboardStatusDistributionItem struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Value  int64  `json:"value"`
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
	TotalEpisodes    sql.NullInt64   `db:"total_episodes"`
	TotalDurationSec sql.NullFloat64 `db:"total_duration_sec"`
	TotalDataSize    sql.NullInt64   `db:"total_data_size_bytes"`
}

type dashboardProductionTotals struct {
	TotalEpisodes        int64
	TotalDataDurationSec float64
	TotalDataSizeBytes   int64
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

type dashboardRecentTaskItem struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	TaskName    string `json:"task_name"`
	Status      string `json:"status"`
	StatusText  string `json:"status_text"`
	RobotName   string `json:"robot_name"`
	StationName string `json:"station_name"`
	DCPlanID    string `json:"dc_plan_id"`
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
	DCPlanID    string       `db:"dc_plan_id"`
	EpisodeID   string       `db:"episode_id"`
	CreatedAt   sql.NullTime `db:"created_at"`
	UpdatedAt   sql.NullTime `db:"updated_at"`
}

type dashboardPreviewItem struct {
	ID              string  `json:"id"`
	DCPlanName      string  `json:"dc_plan_name"`
	DCType          string  `json:"dc_type"`
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
	DCPlanName      string          `db:"dc_plan_name"`
	DCType          string          `db:"dc_type"`
	DeviceID        string          `db:"device_id"`
	StationName     string          `db:"station_name"`
	Status          string          `db:"status"`
	McapPath        string          `db:"mcap_path"`
	DurationSeconds sql.NullFloat64 `db:"duration_seconds"`
	CreatedAt       sql.NullTime    `db:"created_at"`
	EpisodeID       string          `db:"episode_id"`
	TaskID          string          `db:"task_id"`
}

type dashboardDB interface {
	Get(dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
}

// GetOverview returns the production big-screen aggregate contract.
//
// @Summary      Get production dashboard overview
// @Description  Returns one aggregate overview for the Synapse production big screen.
// @Tags         production-dashboard
// @Accept       json
// @Produce      json
// @Param        workstation_id query int false "Filter by workstation ID; ignored for data_collector"
// @Param        organization_id query int false "Filter by organization ID; ignored for data_collector"
// @Param        timezone_offset query string false "Timezone offset such as +08:00"
// @Param        trend_days query int false "Trend day count (default 7, max 31)"
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
	overviewTaskMetrics, err := h.dashboardOverviewTaskMetrics(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] overview task metric query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	todayProductionTotals, err := h.dashboardTodayProductionTotals(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] overview today production query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	trend, err := h.dashboardDataProductionTrend(tx, scope, q)
	if err != nil {
		logger.Printf("[DASHBOARD] overview data production trend query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get production dashboard overview"})
		return
	}
	quality, productionTotals, err := h.dashboardQuality(tx, scope)
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
	activePlans, err := h.dashboardActivePlanCount(tx, scope)
	if err != nil {
		logger.Printf("[DASHBOARD] overview active plan query failed: %v", err)
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
		Summary:                buildOverviewSummary(tasks, overviewTaskMetrics, todayProductionTotals, quality, productionTotals, devices, stationsOverview, activePlans),
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

func parseProductionDashboardQuery(c *gin.Context) (productionDashboardQuery, error) {
	workstationID, err := parseOptionalPositiveIDQuery(c, "workstation_id")
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
		WorkstationID:  workstationID,
		OrganizationID: orgID,
		TrendDays:      trendDays,
		RecentLimit:    recentLimit,
		PreviewLimit:   previewLimit,
		TimezoneOffset: timezoneOffset,
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
		OrganizationID: q.OrganizationID,
		collectorID:    claims.CollectorID,
	}

	switch claims.Role {
	case "admin", "display":
		return scope, nil
	case "data_collector":
		workspaceIDs, err := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, claims.OperatorID)
		if err != nil {
			return productionDashboardScope{}, err
		}
		if len(workspaceIDs) == 0 {
			scope.Warning = "workspace access denied"
			scope.empty = true
			return scope, nil
		}
		scope.workspaceIDs = workspaceIDs
		query, args, err := sqlx.In(`
			SELECT COUNT(*)
			FROM workstations
			WHERE data_collector_id = ? AND workspace_id IN (?) AND is_current = TRUE AND deleted_at IS NULL
		`, claims.CollectorID, workspaceIDs)
		if err != nil {
			return productionDashboardScope{}, err
		}
		var workstationCount int
		err = h.db.GetContext(c.Request.Context(), &workstationCount, h.db.Rebind(query), args...)
		if err != nil {
			return productionDashboardScope{}, err
		}
		if workstationCount == 0 {
			scope.WorkstationID = ""
			scope.Warning = "workstation not assigned"
			scope.empty = true
			return scope, nil
		}
		// Collector dashboard queries use collector ownership and aggregate all current
		// and historical workstations instead of exposing an arbitrary current one.
		scope.WorkstationID = ""
		scope.OrganizationID = ""
		return scope, nil
	default:
		return productionDashboardScope{}, fmt.Errorf("unsupported role")
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
			COALESCE(SUM(CASE WHEN t.status = 'uploading' THEN 1 ELSE 0 END), 0) AS uploading,
			COALESCE(SUM(CASE WHEN t.status = 'pending' THEN 1 ELSE 0 END), 0) AS pending,
			COALESCE(SUM(CASE WHEN t.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready,
			COALESCE(SUM(CASE WHEN t.status = 'failed' THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN t.status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardTaskCounts
	return row, db.Get(&row, query, args...)
}

func (h *ProductionDashboardHandler) dashboardOverviewTaskMetrics(db dashboardDB, scope productionDashboardScope, q productionDashboardQuery) (dashboardTaskCounts, error) {
	location := fixedZoneFromOffset(q.TimezoneOffset)
	nowLocal := time.Now().In(location)
	startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, location)
	endLocal := startLocal.AddDate(0, 0, 1)
	startUTC := startLocal.UTC()
	endUTC := endLocal.UTC()

	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{startUTC, endUTC, startUTC, endUTC, startUTC, endUTC}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN t.assigned_at >= ? AND t.assigned_at < ? THEN 1 ELSE 0 END), 0) AS total,
			COALESCE(SUM(CASE WHEN t.status = 'completed' AND t.completed_at >= ? AND t.completed_at < ? THEN 1 ELSE 0 END), 0) AS completed,
			COALESCE(SUM(CASE WHEN t.status = 'in_progress' THEN 1 ELSE 0 END), 0) AS in_progress,
			COALESCE(SUM(CASE WHEN t.status = 'uploading' THEN 1 ELSE 0 END), 0) AS uploading,
			COALESCE(SUM(CASE WHEN t.status = 'pending' THEN 1 ELSE 0 END), 0) AS pending,
			0 AS ready,
			COALESCE(SUM(CASE WHEN t.status = 'failed' AND t.completed_at >= ? AND t.completed_at < ? THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN t.status = 'cancelled' THEN 1 ELSE 0 END), 0) AS cancelled
		FROM tasks t
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardTaskCounts
	return row, db.Get(&row, query, args...)
}

func (h *ProductionDashboardHandler) dashboardTodayProductionTotals(db dashboardDB, scope productionDashboardScope, q productionDashboardQuery) (dashboardProductionTotals, error) {
	location := fixedZoneFromOffset(q.TimezoneOffset)
	nowLocal := time.Now().In(location)
	startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, location)
	endLocal := startLocal.AddDate(0, 0, 1)
	startUTC := startLocal.UTC()
	endUTC := endLocal.UTC()

	eventTimeExpr := "COALESCE(t.completed_at, e.created_at)"
	conditions := []string{"e.deleted_at IS NULL", eventTimeExpr + " >= ?", eventTimeExpr + " < ?"}
	args := []interface{}{startUTC, endUTC}
	conditions, args = appendDashboardEpisodeScope(conditions, args, scope)

	query := `
		SELECT
			COUNT(e.id) AS total_episodes,
			COALESCE(SUM(COALESCE(e.duration_sec, 0)), 0) AS total_duration_sec,
			COALESCE(SUM(COALESCE(e.file_size_bytes, 0)), 0) AS total_data_size_bytes
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardQualityRow
	if err := db.Get(&row, query, args...); err != nil {
		return dashboardProductionTotals{}, err
	}
	return dashboardProductionTotals{
		TotalEpisodes:        nullInt64(row.TotalEpisodes),
		TotalDataDurationSec: nullFloat64(row.TotalDurationSec),
		TotalDataSizeBytes:   nullInt64(row.TotalDataSize),
	}, nil
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

func (h *ProductionDashboardHandler) dashboardQuality(db dashboardDB, scope productionDashboardScope) (dashboardQuality, dashboardProductionTotals, error) {
	conditions := []string{"e.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardEpisodeScope(conditions, args, scope)
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN e.qa_status <> 'pending_qa' THEN 1 ELSE 0 END), 0) AS total_inspected,
			COALESCE(SUM(CASE WHEN e.qa_status = 'approved' THEN 1 ELSE 0 END), 0) AS passed,
			COALESCE(SUM(CASE WHEN e.qa_status = 'failed' THEN 1 ELSE 0 END), 0) AS failed,
			COALESCE(SUM(CASE WHEN e.qa_status = 'qa_running' THEN 1 ELSE 0 END), 0) AS inspecting,
			COALESCE(SUM(CASE WHEN e.qa_status = 'pending_qa' THEN 1 ELSE 0 END), 0) AS pending_qa,
			COUNT(1) AS total_episodes,
			COALESCE(SUM(COALESCE(e.duration_sec, 0)), 0) AS total_duration_sec,
			COALESCE(SUM(COALESCE(e.file_size_bytes, 0)), 0) AS total_data_size_bytes
		FROM episodes e
		WHERE ` + strings.Join(conditions, " AND ")

	var row dashboardQualityRow
	if err := db.Get(&row, query, args...); err != nil {
		return dashboardQuality{}, dashboardProductionTotals{}, err
	}
	inspected := nullInt64(row.TotalInspected)
	passed := nullInt64(row.Passed)
	passRate := 0.0
	if inspected > 0 {
		passRate = math.Round((float64(passed)/float64(inspected))*1000) / 10
	}
	quality := dashboardQuality{
		PassRate:       passRate,
		TotalInspected: inspected,
		Passed:         passed,
		Failed:         nullInt64(row.Failed),
		Inspecting:     nullInt64(row.Inspecting),
		PendingQA:      nullInt64(row.PendingQA),
	}
	productionTotals := dashboardProductionTotals{
		TotalEpisodes:        nullInt64(row.TotalEpisodes),
		TotalDataDurationSec: nullFloat64(row.TotalDurationSec),
		TotalDataSizeBytes:   nullInt64(row.TotalDataSize),
	}
	return quality, productionTotals, nil
}

func (h *ProductionDashboardHandler) dashboardStations(db dashboardDB, scope productionDashboardScope) ([]dashboardStationItem, error) {
	conditions := []string{"ws.deleted_at IS NULL", "ws.is_current = TRUE"}
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

func (h *ProductionDashboardHandler) dashboardActivePlanCount(db dashboardDB, scope productionDashboardScope) (int64, error) {
	conditions := []string{
		"t.deleted_at IS NULL",
		"t.dc_plan_id IS NOT NULL",
		"t.status IN ('pending', 'ready', 'in_progress', 'uploading')",
	}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)

	var count int64
	err := db.Get(&count, `
		SELECT COUNT(DISTINCT t.dc_plan_id)
		FROM tasks t
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		WHERE `+strings.Join(conditions, " AND "), args...)
	return count, err
}

func (h *ProductionDashboardHandler) dashboardRecentTasks(db dashboardDB, scope productionDashboardScope, limit int) ([]dashboardRecentTaskItem, error) {
	conditions := []string{"t.deleted_at IS NULL"}
	args := []interface{}{}
	conditions, args = appendDashboardTaskScope(conditions, args, scope)
	updatedAtExpr := dashboardRecentTaskUpdatedAtSQL("t")
	query := `
		SELECT
			CAST(t.id AS CHAR) AS id,
			COALESCE(NULLIF(t.task_id, ''), CAST(t.id AS CHAR)) AS task_id,
			COALESCE(NULLIF(dp.name, ''), NULLIF(t.task_id, ''), CAST(t.id AS CHAR)) AS task_name,
			COALESCE(t.status, '') AS status,
			COALESCE(ws.robot_name, ws.robot_serial, '') AS robot_name,
			COALESCE(ws.name, CAST(ws.id AS CHAR), '') AS station_name,
			COALESCE(CAST(t.dc_plan_id AS CHAR), '') AS dc_plan_id,
			COALESCE(e.episode_id, '') AS episode_id,
			t.created_at AS created_at,
			` + updatedAtExpr + ` AS updated_at
		FROM tasks t
		LEFT JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
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
			DCPlanID:    row.DCPlanID,
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
	query := `
		SELECT
			CAST(e.id AS CHAR) AS id,
			COALESCE(NULLIF(dp.name, ''), NULLIF(t.task_id, ''), '') AS dc_plan_name,
			COALESCE(NULLIF(dp.dc_type, ''), '') AS dc_type,
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
		LEFT JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
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
			DCPlanName:      row.DCPlanName,
			DCType:          row.DCType,
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
	overviewTaskMetrics dashboardTaskCounts,
	todayProductionTotals dashboardProductionTotals,
	quality dashboardQuality,
	productionTotals dashboardProductionTotals,
	devices dashboardOverviewDevices,
	stations dashboardOverviewStations,
	activePlanCount int64,
) dashboardOverviewSummary {
	return dashboardOverviewSummary{
		TotalTasks:           tasks.Total,
		TotalEpisodes:        productionTotals.TotalEpisodes,
		TodayTasks:           overviewTaskMetrics.Total,
		TodayEpisodes:        todayProductionTotals.TotalEpisodes,
		CompletedTasks:       overviewTaskMetrics.Completed,
		InProgressTasks:      overviewTaskMetrics.InProgress,
		UploadingTasks:       overviewTaskMetrics.Uploading,
		PendingTasks:         overviewTaskMetrics.Pending,
		FailedTasks:          overviewTaskMetrics.Failed,
		CancelledTasks:       tasks.Cancelled,
		ActivePlans:          activePlanCount,
		ActiveStations:       stations.Summary.Active,
		TotalStations:        stations.Summary.Total,
		RobotOnlineRate:      devices.Summary.OnlineRate,
		QualityPassRate:      quality.PassRate,
		TotalDataDurationSec: productionTotals.TotalDataDurationSec,
		TotalDataSizeBytes:   productionTotals.TotalDataSizeBytes,
		TodayDataDurationSec: todayProductionTotals.TotalDataDurationSec,
		TodayDataSizeBytes:   todayProductionTotals.TotalDataSizeBytes,
	}
}

func buildTaskStatusDistribution(tasks dashboardTaskCounts) []dashboardStatusDistributionItem {
	items := []dashboardStatusDistributionItem{
		{Status: "pending", Label: dashboardTaskStatusText("pending"), Value: tasks.Pending},
		{Status: "ready", Label: dashboardTaskStatusText("ready"), Value: tasks.Ready},
		{Status: "in_progress", Label: dashboardTaskStatusText("in_progress"), Value: tasks.InProgress},
		{Status: "uploading", Label: dashboardTaskStatusText("uploading"), Value: tasks.Uploading},
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
		workspaceCondition, workspaceArgs := dashboardInt64InCondition("ws_scope.workspace_id", scope.workspaceIDs)
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.id = t.workstation_id
				AND ws_scope.data_collector_id = ?
				AND `+workspaceCondition+`
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		args = append(args, workspaceArgs...)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(t.workstation_id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(t.organization_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func appendDashboardEpisodeScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		workspaceCondition, workspaceArgs := dashboardInt64InCondition("ws_scope.workspace_id", scope.workspaceIDs)
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.id = e.workstation_id
				AND ws_scope.data_collector_id = ?
				AND `+workspaceCondition+`
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		args = append(args, workspaceArgs...)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(e.workstation_id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
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
		workspaceCondition, workspaceArgs := dashboardInt64InCondition("ws.workspace_id", scope.workspaceIDs)
		conditions = append(conditions, workspaceCondition)
		args = append(args, workspaceArgs...)
		return conditions, args
	}
	if scope.WorkstationID != "" {
		conditions = append(conditions, "CAST(ws.id AS CHAR) = ?")
		args = append(args, scope.WorkstationID)
	}
	if scope.OrganizationID != "" {
		conditions = append(conditions, "CAST(ws.workspace_id AS CHAR) = ?")
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func appendDashboardRobotScope(conditions []string, args []interface{}, scope productionDashboardScope) ([]string, []interface{}) {
	if scope.Role == "data_collector" {
		workspaceCondition, workspaceArgs := dashboardInt64InCondition("ws_scope.workspace_id", scope.workspaceIDs)
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.robot_id = r.id
				AND ws_scope.data_collector_id = ?
				AND `+workspaceCondition+`
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.collectorID)
		args = append(args, workspaceArgs...)
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
	if scope.OrganizationID != "" {
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM workstations ws_scope
			WHERE ws_scope.robot_id = r.id
				AND CAST(ws_scope.workspace_id AS CHAR) = ?
				AND ws_scope.deleted_at IS NULL
		)`)
		args = append(args, scope.OrganizationID)
	}
	return conditions, args
}

func dashboardInt64InCondition(column string, values []int64) (string, []interface{}) {
	if len(values) == 0 {
		return "1 = 0", nil
	}
	placeholders := make([]string, 0, len(values))
	args := make([]interface{}, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
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
	case "uploading":
		return "上传中"
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
