// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// DCPlanHandler handles Hilbert dc_plan projection requests.
type DCPlanHandler struct {
	db          *sqlx.DB
	syncService *services.DCPlanSyncService
}

// NewDCPlanHandler creates a new DCPlanHandler.
func NewDCPlanHandler(db *sqlx.DB, syncService *services.DCPlanSyncService) *DCPlanHandler {
	return &DCPlanHandler{db: db, syncService: syncService}
}

// DCPlanResponse represents one Hilbert dc_plan projection.
type DCPlanResponse struct {
	ID                  int64  `json:"id"`
	WorkspaceID         int64  `json:"workspace_id"`
	Name                string `json:"name"`
	Description         string `json:"description,omitempty"`
	DCFactoryID         int64  `json:"dc_factory_id"`
	DCServiceProviderID int64  `json:"dc_service_provider_id"`
	Operator            string `json:"operator"`
	DCProjectID         int64  `json:"dc_project_id"`
	DCTaskID            int64  `json:"dc_task_id"`
	DCDeviceID          int64  `json:"dc_device_id"`
	DCType              string `json:"dc_type"`
	DCDate              string `json:"dc_date"`
	TargetCount         int64  `json:"target_count"`
	CurCount            int64  `json:"cur_count"`
	TargetDuration      int64  `json:"target_duration"`
	CurDuration         int64  `json:"cur_duration"`
	CreatedBy           string `json:"created_by"`
	CreatedTime         string `json:"created_time"`
	UpdatedBy           string `json:"updated_by,omitempty"`
	UpdatedTime         string `json:"updated_time,omitempty"`
	LastSyncedAt        string `json:"last_synced_at,omitempty"`
}

// DCPlanListResponse represents a paginated dc_plan list response.
type DCPlanListResponse struct {
	Items   []DCPlanResponse `json:"items"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	HasNext bool             `json:"hasNext,omitempty"`
	HasPrev bool             `json:"hasPrev,omitempty"`
}

// DCPlanSyncResponse represents a manual dc_plan sync result.
type DCPlanSyncResponse struct {
	WorkspaceID    int64                                 `json:"workspace_id"`
	SyncedCount    int                                   `json:"synced_count"`
	PageCount      int                                   `json:"page_count"`
	LastSyncedAt   string                                `json:"last_synced_at"`
	TaskGeneration *services.DCPlanTaskGenerationSummary `json:"task_generation,omitempty"`
}

type dcPlanRow struct {
	ID                  int64          `db:"id"`
	WorkspaceID         int64          `db:"workspace_id"`
	Name                string         `db:"name"`
	Description         sql.NullString `db:"description"`
	DCFactoryID         int64          `db:"dc_factory_id"`
	DCServiceProviderID int64          `db:"dc_service_provider_id"`
	Operator            string         `db:"operator"`
	DCProjectID         int64          `db:"dc_project_id"`
	DCTaskID            int64          `db:"dc_task_id"`
	DCDeviceID          int64          `db:"dc_device_id"`
	DCType              string         `db:"dc_type"`
	DCDate              string         `db:"dc_date"`
	TargetCount         int64          `db:"target_count"`
	CurCount            int64          `db:"cur_count"`
	TargetDuration      int64          `db:"target_duration"`
	CurDuration         int64          `db:"cur_duration"`
	CreatedBy           string         `db:"created_by"`
	CreatedTime         sql.NullTime   `db:"created_time"`
	UpdatedBy           sql.NullString `db:"updated_by"`
	UpdatedTime         sql.NullTime   `db:"updated_time"`
	LastSyncedAt        sql.NullTime   `db:"last_synced_at"`
}

// RegisterRoutes registers dc_plan routes.
func (h *DCPlanHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/dc-plans", h.ListDCPlans)
	apiV1.POST("/workspaces/:workspace_id/dc-plans/sync", h.SyncWorkspaceDCPlans)
}

// ListDCPlans handles Hilbert dc_plan projection listing.
//
// @Summary      List Hilbert dc plans
// @Description  Lists locally cached Hilbert dc_plan projections. workspace_id is required.
// @Tags         dc-plans
// @Accept       json
// @Produce      json
// @Param        workspace_id query int    true  "Workspace ID"
// @Param        name         query string false "Fuzzy plan name filter"
// @Param        dc_type      query string false "Exact data collection type"
// @Param        operator     query string false "Exact operator account code"
// @Param        dc_date      query string false "Exact collection date, YYYY-MM-DD"
// @Param        limit        query int    false "Max results (default 50, max 100)"
// @Param        offset       query int    false "Pagination offset (default 0)"
// @Success      200 {object} DCPlanListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /dc-plans [get]
func (h *DCPlanHandler) ListDCPlans(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	workspaceID, ok := parseRequiredPositiveQueryInt64(c, "workspace_id")
	if !ok {
		return
	}

	name := strings.TrimSpace(c.Query("name"))
	dcType := strings.TrimSpace(c.Query("dc_type"))
	operator := strings.TrimSpace(c.Query("operator"))
	dcDate := strings.TrimSpace(c.Query("dc_date"))
	if dcDate != "" {
		if parsed, err := time.Parse("2006-01-02", dcDate); err != nil || parsed.Format("2006-01-02") != dcDate {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dc_date format"})
			return
		}
	}

	whereClause := "WHERE deleted_at IS NULL AND workspace_id = ?"
	args := []any{workspaceID}
	whereClause, args = appendKeywordSearch(whereClause, args, name, "name")
	if dcType != "" {
		whereClause += " AND dc_type = ?"
		args = append(args, dcType)
	}
	if operator != "" {
		whereClause += " AND operator = ?"
		args = append(args, operator)
	}
	if dcDate != "" {
		whereClause += " AND dc_date = ?"
		args = append(args, dcDate)
	}

	var total int
	if err := h.db.Get(&total, "SELECT COUNT(*) FROM dc_plan "+whereClause, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to count dc plans: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
		return
	}

	query := `
		SELECT
			id, workspace_id, name, description, dc_factory_id, dc_service_provider_id,
			operator, dc_project_id, dc_task_id, dc_device_id, dc_type, CAST(dc_date AS CHAR) AS dc_date,
			target_count, cur_count, target_duration, cur_duration, created_by, created_time,
			updated_by, updated_time, last_synced_at
		FROM dc_plan
		` + whereClause + `
		ORDER BY dc_date DESC, id DESC
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var rows []dcPlanRow
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to query dc plans: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
		return
	}

	items := make([]DCPlanResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, dcPlanResponseFromRow(row))
	}

	c.JSON(http.StatusOK, DCPlanListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: (pagination.Offset + pagination.Limit) < total,
		HasPrev: pagination.Offset > 0,
	})
}

// SyncWorkspaceDCPlans handles manual Hilbert dc_plan sync for one workspace.
//
// @Summary      Sync Hilbert dc plans for one workspace
// @Description  Logs in with Keystone's Hilbert service identity and transactionally upserts one workspace's Hilbert dc_plan projections.
// @Tags         dc-plans
// @Accept       json
// @Produce      json
// @Param        workspace_id path int true "Workspace ID"
// @Success      200 {object} DCPlanSyncResponse
// @Failure      400 {object} map[string]string
// @Failure      503 {object} map[string]string
// @Router       /workspaces/{workspace_id}/dc-plans/sync [post]
func (h *DCPlanHandler) SyncWorkspaceDCPlans(c *gin.Context) {
	if h.syncService == nil || !h.syncService.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dc plan sync is not configured"})
		return
	}

	workspaceID, ok := parsePositivePathInt64(c, "workspace_id")
	if !ok {
		return
	}

	result, err := h.syncService.SyncWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		if errors.Is(err, services.ErrDCPlanSyncNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dc plan sync is not configured"})
			return
		}
		if errors.Is(err, services.ErrDCPlanSyncInvalidWorkspace) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace for dc plan sync"})
			return
		}
		logger.Printf("[DC_PLAN] Hilbert dc plan sync failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "dc plan sync failed"})
		return
	}

	c.JSON(http.StatusOK, DCPlanSyncResponse{
		WorkspaceID:    result.WorkspaceID,
		SyncedCount:    result.SyncedCount,
		PageCount:      result.PageCount,
		LastSyncedAt:   result.LastSyncedAt.UTC().Format(time.RFC3339),
		TaskGeneration: result.TaskGeneration,
	})
}

func parseRequiredPositiveQueryInt64(c *gin.Context, field string) (int64, bool) {
	raw := strings.TrimSpace(c.Query(field))
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": field + " is required"})
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + field + " format"})
		return 0, false
	}
	return value, true
}

func parsePositivePathInt64(c *gin.Context, field string) (int64, bool) {
	raw := strings.TrimSpace(c.Param(field))
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + field + " format"})
		return 0, false
	}
	return value, true
}

func dcPlanResponseFromRow(row dcPlanRow) DCPlanResponse {
	return DCPlanResponse{
		ID:                  row.ID,
		WorkspaceID:         row.WorkspaceID,
		Name:                row.Name,
		Description:         row.Description.String,
		DCFactoryID:         row.DCFactoryID,
		DCServiceProviderID: row.DCServiceProviderID,
		Operator:            row.Operator,
		DCProjectID:         row.DCProjectID,
		DCTaskID:            row.DCTaskID,
		DCDeviceID:          row.DCDeviceID,
		DCType:              row.DCType,
		DCDate:              row.DCDate,
		TargetCount:         row.TargetCount,
		CurCount:            row.CurCount,
		TargetDuration:      row.TargetDuration,
		CurDuration:         row.CurDuration,
		CreatedBy:           row.CreatedBy,
		CreatedTime:         formatWorkspaceNullableTime(row.CreatedTime),
		UpdatedBy:           row.UpdatedBy.String,
		UpdatedTime:         formatWorkspaceNullableTime(row.UpdatedTime),
		LastSyncedAt:        formatWorkspaceNullableTime(row.LastSyncedAt),
	}
}
