// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
)

// DCPlanProjectOption represents one distinct Hilbert project option.
type DCPlanProjectOption struct {
	DCProjectID   int64  `json:"dc_project_id"`
	DCProjectName string `json:"dc_project_name"`
}

// DCPlanProjectOptionListResponse represents a paginated list of project options.
type DCPlanProjectOptionListResponse struct {
	Items   []DCPlanProjectOption `json:"items"`
	Total   int                   `json:"total"`
	Limit   int                   `json:"limit"`
	Offset  int                   `json:"offset"`
	HasNext bool                  `json:"hasNext,omitempty"`
	HasPrev bool                  `json:"hasPrev,omitempty"`
}

// DCPlanTaskOption represents one distinct Hilbert task option.
type DCPlanTaskOption struct {
	DCTaskID   int64  `json:"dc_task_id"`
	DCTaskName string `json:"dc_task_name"`
}

// DCPlanTaskOptionListResponse represents a paginated list of task options.
type DCPlanTaskOptionListResponse struct {
	Items   []DCPlanTaskOption `json:"items"`
	Total   int                `json:"total"`
	Limit   int                `json:"limit"`
	Offset  int                `json:"offset"`
	HasNext bool               `json:"hasNext,omitempty"`
	HasPrev bool               `json:"hasPrev,omitempty"`
}

// ListDCPlanProjectOptions returns distinct Hilbert project options for remote selects.
func (h *DCPlanHandler) ListDCPlanProjectOptions(c *gin.Context) {
	startedAt := time.Now()
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	workspaceID, ok := parseRequiredPositiveQueryInt64(c, "workspace_id")
	if !ok {
		return
	}
	if !h.dcPlanWorkspaceAccessible(c, workspaceID) {
		return
	}

	keyword := strings.TrimSpace(c.Query("keyword"))
	whereClause := "WHERE dp.deleted_at IS NULL AND dp.workspace_id = ? AND dp.dc_project_id IS NOT NULL"
	args := []any{workspaceID}
	if keyword != "" {
		whereClause, args = appendKeywordSearch(whereClause, args, keyword, "CAST(dp.dc_project_id AS CHAR)", "dp.dc_project_name")
	}

	countQuery := "SELECT COUNT(DISTINCT dp.dc_project_id) FROM dc_plan dp " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to count project options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plan projects"})
		return
	}

	query := `
		SELECT
			dp.dc_project_id,
			MAX(COALESCE(NULLIF(dp.dc_project_name, ''), CAST(dp.dc_project_id AS CHAR))) AS dc_project_name
		FROM dc_plan dp
		` + whereClause + `
		GROUP BY dp.dc_project_id
		ORDER BY dc_project_name, dp.dc_project_id
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var items []DCPlanProjectOption
	if err := h.db.Select(&items, query, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to query project options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plan projects"})
		return
	}

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		logger.Printf("[DC_PLAN] Slow project options query: workspace_id=%d keyword=%q total=%s items=%d", workspaceID, keyword, elapsed, len(items))
	}

	c.JSON(http.StatusOK, DCPlanProjectOptionListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: pagination.Offset+pagination.Limit < total,
		HasPrev: pagination.Offset > 0,
	})
}

// ListDCPlanTaskOptions returns distinct Hilbert task options for remote selects.
func (h *DCPlanHandler) ListDCPlanTaskOptions(c *gin.Context) {
	startedAt := time.Now()
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	workspaceID, ok := parseRequiredPositiveQueryInt64(c, "workspace_id")
	if !ok {
		return
	}
	if !h.dcPlanWorkspaceAccessible(c, workspaceID) {
		return
	}

	keyword := strings.TrimSpace(c.Query("keyword"))
	whereClause := "WHERE dp.deleted_at IS NULL AND dp.workspace_id = ? AND dp.dc_task_id IS NOT NULL"
	args := []any{workspaceID}
	if keyword != "" {
		whereClause, args = appendKeywordSearch(whereClause, args, keyword, "CAST(dp.dc_task_id AS CHAR)", "dp.dc_task_name")
	}

	countQuery := "SELECT COUNT(DISTINCT dp.dc_task_id) FROM dc_plan dp " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to count task options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plan tasks"})
		return
	}

	query := `
		SELECT
			dp.dc_task_id,
			MAX(COALESCE(NULLIF(dp.dc_task_name, ''), CAST(dp.dc_task_id AS CHAR))) AS dc_task_name
		FROM dc_plan dp
		` + whereClause + `
		GROUP BY dp.dc_task_id
		ORDER BY dc_task_name, dp.dc_task_id
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var items []DCPlanTaskOption
	if err := h.db.Select(&items, query, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to query task options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plan tasks"})
		return
	}

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		logger.Printf("[DC_PLAN] Slow task options query: workspace_id=%d keyword=%q total=%s items=%d", workspaceID, keyword, elapsed, len(items))
	}

	c.JSON(http.StatusOK, DCPlanTaskOptionListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: pagination.Offset+pagination.Limit < total,
		HasPrev: pagination.Offset > 0,
	})
}

func (h *DCPlanHandler) dcPlanWorkspaceAccessible(c *gin.Context, workspaceID int64) bool {
	claims := middleware.GetClaims(c)
	if claims != nil && claims.Role == "data_collector" {
		workspaceIDs, err := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, claims.OperatorID)
		if err != nil {
			logger.Printf("[DC_PLAN] Failed to resolve collector Workspace access: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
			return false
		}
		if !int64SliceContains(workspaceIDs, workspaceID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "workspace access denied"})
			return false
		}
	}
	return true
}
