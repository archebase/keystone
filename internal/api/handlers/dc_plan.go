// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// DCPlanHandler handles Hilbert dc_plan projection requests.
type DCPlanHandler struct {
	db                *sqlx.DB
	syncService       dcPlanWorkspaceSyncer
	targetCountClient dcPlanTargetCountClient
	taskSupply        dcPlanTaskSupply
}

type dcPlanWorkspaceSyncer interface {
	Configured() bool
	SyncWorkspace(context.Context, int64) (*services.DCPlanSyncResult, error)
}

type dcPlanTargetCountClient interface {
	PatchDCPlanTargetCount(context.Context, int64, int64, int64) (bool, error)
}

type dcPlanTaskSupply interface {
	TrimPendingTasks(context.Context, int64, time.Time) (int, error)
}

// NewDCPlanHandler creates a new DCPlanHandler.
func NewDCPlanHandler(db *sqlx.DB, syncService dcPlanWorkspaceSyncer, targetCountClient ...dcPlanTargetCountClient) *DCPlanHandler {
	var client dcPlanTargetCountClient
	if len(targetCountClient) > 0 {
		client = targetCountClient[0]
	}
	return &DCPlanHandler{db: db, syncService: syncService, targetCountClient: client, taskSupply: services.NewDCPlanTaskSupplyService(db)}
}

// DCPlanResponse represents one Hilbert dc_plan projection.
type DCPlanResponse struct {
	ID                   int64  `json:"id"`
	WorkspaceID          int64  `json:"workspace_id"`
	Name                 string `json:"name"`
	Description          string `json:"description,omitempty"`
	DCFactoryID          int64  `json:"dc_factory_id"`
	DCServiceProviderID  int64  `json:"dc_service_provider_id"`
	Operator             string `json:"operator"`
	OperatorDisplayName  string `json:"operator_display_name,omitempty"`
	DCProjectID          int64  `json:"dc_project_id"`
	DCProjectName        string `json:"dc_project_name,omitempty"`
	DCProjectDescription string `json:"dc_project_description,omitempty"`
	DCTaskID             int64  `json:"dc_task_id"`
	DCTaskName           string `json:"dc_task_name,omitempty"`
	DCTaskDescription    string `json:"dc_task_description,omitempty"`
	DCDeviceID           int64  `json:"dc_device_id"`
	DCDeviceName         string `json:"dc_device_name,omitempty"`
	DCType               string `json:"dc_type"`
	DCDate               string `json:"dc_date"`
	TargetCount          int64  `json:"target_count"`
	CurCount             int64  `json:"cur_count"`
	TargetDuration       int64  `json:"target_duration"`
	CurDuration          int64  `json:"cur_duration"`
	CreatedBy            string `json:"created_by"`
	CreatedTime          string `json:"created_time"`
	UpdatedBy            string `json:"updated_by,omitempty"`
	UpdatedTime          string `json:"updated_time,omitempty"`
	LastSyncedAt         string `json:"last_synced_at,omitempty"`
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
	WorkspaceID  int64  `json:"workspace_id"`
	SyncedCount  int    `json:"synced_count"`
	PageCount    int    `json:"page_count"`
	LastSyncedAt string `json:"last_synced_at"`
}

// OperatorPlanItem represents one plan currently available to the logged-in collector.
// A plan without a Hilbert device is exposed with the current robot device ID as its effective device so the collector can select it; the actual Hilbert binding still happens when the task is requested.
type OperatorPlanItem struct {
	ID                    int64  `json:"id"`
	WorkspaceID           int64  `json:"workspace_id"`
	Name                  string `json:"name"`
	DCProjectID           int64  `json:"dc_project_id"`
	DCProjectName         string `json:"dc_project_name,omitempty"`
	DCProjectDescription  string `json:"dc_project_description,omitempty"`
	DCTaskID              int64  `json:"dc_task_id"`
	DCTaskName            string `json:"dc_task_name,omitempty"`
	DCTaskDescription     string `json:"dc_task_description,omitempty"`
	DCDeviceID            int64  `json:"dc_device_id"`
	DCDeviceName          string `json:"dc_device_name,omitempty"`
	DCType                string `json:"dc_type"`
	TargetCount           int64  `json:"target_count"`
	CurCount              int64  `json:"cur_count"`
	CloudCurCount         int64  `json:"cloud_cur_count"`
	LocalCurCount         int64  `json:"local_cur_count"`
	LocalPendingCount     int64  `json:"local_pending_count"`
	LocalApprovedCount    int64  `json:"local_approved_count"`
	LocalFailedCount      int64  `json:"local_failed_count"`
	TargetDuration        int64  `json:"target_duration"`
	CurDuration           int64  `json:"cur_duration"`
	CloudCurDuration      int64  `json:"cloud_cur_duration"`
	LocalCurDuration      int64  `json:"local_cur_duration"`
	LocalPendingDuration  int64  `json:"local_pending_duration"`
	LocalApprovedDuration int64  `json:"local_approved_duration"`
	LocalFailedDuration   int64  `json:"local_failed_duration"`
	CommittedCount        int64  `json:"committed_count"`
	RemainingCount        int64  `json:"remaining_count"`
	LastSyncedAt          string `json:"last_synced_at,omitempty"`
}

// OperatorPlanRefreshResponse reports the collector's latest assigned plans.
type OperatorPlanRefreshResponse struct {
	Items        []OperatorPlanItem `json:"items"`
	Stale        bool               `json:"stale"`
	LastSyncedAt string             `json:"last_synced_at,omitempty"`
}

type dcPlanRow struct {
	ID                   int64           `db:"id"`
	WorkspaceID          int64           `db:"workspace_id"`
	Name                 string          `db:"name"`
	Description          sql.NullString  `db:"description"`
	DCFactoryID          int64           `db:"dc_factory_id"`
	DCServiceProviderID  int64           `db:"dc_service_provider_id"`
	Operator             string          `db:"operator"`
	OperatorDisplayName  sql.NullString  `db:"operator_display_name"`
	DCProjectID          int64           `db:"dc_project_id"`
	DCProjectName        sql.NullString  `db:"dc_project_name"`
	DCProjectDescription sql.NullString  `db:"dc_project_description"`
	DCTaskID             int64           `db:"dc_task_id"`
	DCTaskName           sql.NullString  `db:"dc_task_name"`
	DCTaskDescription    sql.NullString  `db:"dc_task_description"`
	DCDeviceID           sql.NullInt64   `db:"dc_device_id"`
	DCDeviceName         sql.NullString  `db:"dc_device_name"`
	DCType               string          `db:"dc_type"`
	DCDate               string          `db:"dc_date"`
	TargetCount          int64           `db:"target_count"`
	CurCount             int64           `db:"cur_count"`
	LocalCurCount        int64           `db:"local_cur_count"`
	TargetDuration       int64           `db:"target_duration"`
	CurDuration          int64           `db:"cur_duration"`
	LocalCurDuration     sql.NullFloat64 `db:"local_cur_duration"`
	CreatedBy            string          `db:"created_by"`
	CreatedTime          sql.NullTime    `db:"created_time"`
	UpdatedBy            sql.NullString  `db:"updated_by"`
	UpdatedTime          sql.NullTime    `db:"updated_time"`
	LastSyncedAt         sql.NullTime    `db:"last_synced_at"`
}

// RegisterRoutes registers dc_plan routes.
func (h *DCPlanHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/dc-plans", h.ListDCPlans)
	apiV1.POST("/workspaces/:workspace_id/dc-plans/sync", h.SyncWorkspaceDCPlans)
}

// RegisterReadRoutes registers dc_plan routes available to authenticated readers.
func (h *DCPlanHandler) RegisterReadRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/dc-plans", h.ListDCPlans)
	apiV1.GET("/dc-plans/options/projects", h.ListDCPlanProjectOptions)
	apiV1.GET("/dc-plans/options/tasks", h.ListDCPlanTaskOptions)
	apiV1.POST("/operator/plans/refresh", h.RefreshOperatorPlans)
}

// RegisterOperatorRoutes registers dc_plan routes available to an authenticated workstation.
func (h *DCPlanHandler) RegisterOperatorRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/operator/dc-plans/:plan_id/target-count", h.UpdateTargetCount)
}

// UpdateTargetCount updates a plan target count through Hilbert after verifying the active workstation owns the plan.
// The lower bound is the number of non-deleted Keystone episodes already associated with the plan.
//
// @Summary      Update operator dc plan target count
// @Description  Updates a Hilbert dc plan target count using the active workstation session. The new target cannot be below the number of non-deleted Keystone episodes for the plan.
// @Tags         dc-plans
// @Accept       json
// @Produce      json
// @Param        plan_id path int true "Hilbert dc plan ID"
// @Param        body body UpdateTargetCountRequest true "Target count update"
// @Success      200 {object} UpdateTargetCountResponse
// @Failure      400 {object} map[string]any
// @Failure      401 {object} map[string]any
// @Failure      403 {object} map[string]any
// @Failure      404 {object} map[string]any
// @Failure      409 {object} map[string]any
// @Failure      502 {object} map[string]any
// @Router       /operator/dc-plans/{plan_id}/target-count [post]
func (h *DCPlanHandler) UpdateTargetCount(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" || claims.WorkstationID <= 0 || claims.WorkspaceID <= 0 || strings.TrimSpace(claims.OperatorID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"code": "workstation_scope_required", "error": "workstation session is required"})
		return
	}
	if h.targetCountClient == nil {
		c.JSON(http.StatusBadGateway, gin.H{"code": "hilbert_unavailable", "error": "Hilbert target count client is unavailable"})
		return
	}

	planID, err := strconv.ParseInt(strings.TrimSpace(c.Param("plan_id")), 10, 64)
	if err != nil || planID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_plan_id", "error": "plan_id must be a positive integer"})
		return
	}
	var req UpdateTargetCountRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.TargetCount < 1 || req.TargetCount > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "invalid_target_count", "error": "target_count must be between 1 and 200"})
		return
	}

	var workstation struct {
		WorkspaceID int64  `db:"workspace_id"`
		RobotID     int64  `db:"robot_id"`
		DeviceID    string `db:"device_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &workstation, `
		SELECT ws.workspace_id, ws.robot_id, r.device_id
		FROM workstations ws
		JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE ws.id = ? AND ws.data_collector_id = ? AND ws.is_current = TRUE AND ws.deleted_at IS NULL
		LIMIT 1
	`, claims.WorkstationID, claims.CollectorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"code": "workstation_session_invalid", "error": "workstation session is no longer active"})
			return
		}
		logger.Printf("[DC_PLAN] Failed to resolve target-count workstation: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate workstation session"})
		return
	}
	if workstation.WorkspaceID != claims.WorkspaceID || workstation.RobotID != claims.RobotID {
		c.JSON(http.StatusForbidden, gin.H{"code": "workstation_workspace_mismatch", "error": "workstation workspace mismatch"})
		return
	}
	deviceID, err := strconv.ParseInt(strings.TrimSpace(workstation.DeviceID), 10, 64)
	if err != nil || deviceID <= 0 {
		c.JSON(http.StatusConflict, gin.H{"code": "robot_not_linked_to_hilbert", "error": "robot is not linked to a Hilbert device"})
		return
	}

	var plan struct {
		WorkspaceID int64         `db:"workspace_id"`
		Operator    string        `db:"operator"`
		Status      string        `db:"status"`
		DCDeviceID  sql.NullInt64 `db:"dc_device_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &plan, `
		SELECT workspace_id, operator, COALESCE(status, 'pending_collection') AS status, dc_device_id
		FROM dc_plan
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"code": "dc_plan_not_found", "error": "dc plan not found"})
			return
		}
		logger.Printf("[DC_PLAN] Failed to query target-count plan: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query dc plan"})
		return
	}
	if plan.WorkspaceID != claims.WorkspaceID || plan.Operator != claims.OperatorID {
		c.JSON(http.StatusForbidden, gin.H{"code": "dc_plan_access_denied", "error": "dc plan access denied"})
		return
	}
	if plan.DCDeviceID.Valid && plan.DCDeviceID.Int64 != deviceID {
		c.JSON(http.StatusForbidden, gin.H{"code": "dc_plan_device_mismatch", "error": "dc plan is assigned to another device"})
		return
	}
	if plan.Status != "pending_collection" && plan.Status != "collecting" {
		c.JSON(http.StatusConflict, gin.H{"code": "dc_plan_not_editable", "error": "dc plan cannot be updated in its current state"})
		return
	}

	var uploadedCount int64
	if err := h.db.GetContext(c.Request.Context(), &uploadedCount, `
		SELECT COUNT(*) FROM episodes WHERE dc_plan_id = ? AND deleted_at IS NULL
	`, planID); err != nil {
		logger.Printf("[DC_PLAN] Failed to count target-count episodes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count uploaded episodes"})
		return
	}
	if req.TargetCount < uploadedCount {
		c.JSON(http.StatusConflict, gin.H{
			"code":           "target_count_below_uploaded_count",
			"error":          "target count cannot be less than uploaded count",
			"plan_id":        planID,
			"target_count":   req.TargetCount,
			"uploaded_count": uploadedCount,
		})
		return
	}

	updated, err := h.targetCountClient.PatchDCPlanTargetCount(c.Request.Context(), claims.WorkspaceID, planID, req.TargetCount)
	if err != nil {
		logger.Printf("[DC_PLAN] Failed to patch Hilbert target count: workspace_id=%d plan_id=%d target_count=%d err=%v", claims.WorkspaceID, planID, req.TargetCount, err)
		c.JSON(http.StatusBadGateway, gin.H{"code": "hilbert_unavailable", "error": "failed to update Hilbert dc plan target count"})
		return
	}
	if !updated {
		c.JSON(http.StatusBadGateway, gin.H{"code": "hilbert_update_not_applied", "error": "Hilbert did not update the dc plan target count"})
		return
	}
	if _, err := h.db.ExecContext(c.Request.Context(), `
		UPDATE dc_plan SET target_count = ?, local_updated_at = ?
		WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL
	`, req.TargetCount, time.Now().UTC(), planID, claims.WorkspaceID); err != nil {
		logger.Printf("[DC_PLAN] Hilbert target count updated but local projection failed: workspace_id=%d plan_id=%d err=%v", claims.WorkspaceID, planID, err)
		c.JSON(http.StatusOK, UpdateTargetCountResponse{PlanID: planID, TargetCount: req.TargetCount, UploadedCount: uploadedCount, Changed: true, ProjectionStale: true})
		return
	}

	// 目标数量变更成功后收敛任务池，取消超出新预算的待执行任务。
	tasksCancelled := 0
	if h.taskSupply != nil {
		cancelled, trimErr := h.taskSupply.TrimPendingTasks(c.Request.Context(), planID, time.Now().UTC())
		if trimErr != nil {
			logger.Printf("[DC_PLAN] Target count updated but pending task trim failed: workspace_id=%d plan_id=%d err=%v", claims.WorkspaceID, planID, trimErr)
		} else {
			tasksCancelled = cancelled
		}
	}
	c.JSON(http.StatusOK, UpdateTargetCountResponse{PlanID: planID, TargetCount: req.TargetCount, UploadedCount: uploadedCount, Changed: true, TasksCancelled: tasksCancelled})
}

// UpdateTargetCountRequest contains the new Hilbert dc plan target count.
type UpdateTargetCountRequest struct {
	TargetCount int64 `json:"target_count" binding:"required,gte=1,lte=200"`
}

// UpdateTargetCountResponse reports the applied target, Keystone episode count and cancelled pending tasks.
type UpdateTargetCountResponse struct {
	PlanID          int64 `json:"plan_id"`
	TargetCount     int64 `json:"target_count"`
	UploadedCount   int64 `json:"uploaded_count"`
	TasksCancelled  int   `json:"tasks_cancelled"`
	Changed         bool  `json:"changed"`
	ProjectionStale bool  `json:"projection_stale"`
}

// RefreshOperatorPlans synchronizes and returns plans available to the authenticated workstation.
// It includes plans already bound to the current device and plans without a device whose operator matches the logged-in collector; the latter are bound when the device requests a task.
//
// @Summary      Refresh operator plans
// @Description  Synchronizes Hilbert plans and returns plans assigned to the authenticated collector workstation.
// @Tags         dc-plans
// @Produce      json
// @Success      200 {object} OperatorPlanRefreshResponse
// @Failure      403 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /operator/plans/refresh [post]
func (h *DCPlanHandler) RefreshOperatorPlans(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" || claims.WorkspaceID <= 0 ||
		claims.RobotID <= 0 || claims.WorkstationID <= 0 || strings.TrimSpace(claims.OperatorID) == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "data collector workstation required"})
		return
	}

	var robotDeviceID string
	if err := h.db.GetContext(c.Request.Context(), &robotDeviceID, `
		SELECT device_id
		FROM robots
		WHERE id = ? AND deleted_at IS NULL
	`, claims.RobotID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "robot not found"})
			return
		}
		logger.Printf("[DC_PLAN] Failed to resolve collector robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to refresh operator plans"})
		return
	}
	dcDeviceID, err := strconv.ParseInt(strings.TrimSpace(robotDeviceID), 10, 64)
	if err != nil || dcDeviceID <= 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "robot is not linked to a Hilbert device"})
		return
	}

	stale := false
	if h.syncService == nil {
		stale = true
	} else if _, err := h.syncService.SyncWorkspace(c.Request.Context(), claims.WorkspaceID); err != nil {
		stale = true
		logger.Printf(
			"[DC_PLAN] Operator plan refresh using stale projection: workspace_id=%d operator=%s error=%v",
			claims.WorkspaceID,
			claims.OperatorID,
			err,
		)
	}

	rows := []struct {
		ID                    int64        `db:"id"`
		WorkspaceID           int64        `db:"workspace_id"`
		Name                  string       `db:"name"`
		DCProjectID           int64        `db:"dc_project_id"`
		DCProjectName         string       `db:"dc_project_name"`
		DCProjectDescription  string       `db:"dc_project_description"`
		DCTaskID              int64        `db:"dc_task_id"`
		DCTaskName            string       `db:"dc_task_name"`
		DCTaskDescription     string       `db:"dc_task_description"`
		DCDeviceID            int64        `db:"dc_device_id"`
		DCDeviceName          string       `db:"dc_device_name"`
		DCType                string       `db:"dc_type"`
		TargetCount           int64        `db:"target_count"`
		CurCount              int64        `db:"cur_count"`
		LocalPendingCount     int64        `db:"local_pending_count"`
		LocalApprovedCount    int64        `db:"local_approved_count"`
		LocalFailedCount      int64        `db:"local_failed_count"`
		TargetDuration        int64        `db:"target_duration"`
		CurDuration           int64        `db:"cur_duration"`
		LocalPendingDuration  float64      `db:"local_pending_duration"`
		LocalApprovedDuration float64      `db:"local_approved_duration"`
		LocalFailedDuration   float64      `db:"local_failed_duration"`
		ReservedCount         int64        `db:"reserved_count"`
		LastSyncedAt          sql.NullTime `db:"last_synced_at"`
	}{}
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT
			dp.id,
			dp.workspace_id,
			dp.name,
			dp.dc_project_id,
			COALESCE(dp.dc_project_name, '') AS dc_project_name,
			COALESCE(dp.dc_project_description, '') AS dc_project_description,
			dp.dc_task_id,
			COALESCE(dp.dc_task_name, '') AS dc_task_name,
			COALESCE(dp.dc_task_description, '') AS dc_task_description,
			COALESCE(dp.dc_device_id, ?) AS dc_device_id,
			COALESCE(dp.dc_device_name, '') AS dc_device_name,
			dp.dc_type,
			dp.target_count,
			dp.cur_count,
			dp.target_duration,
			dp.cur_duration,
			dp.last_synced_at,
			COALESCE(progress.local_pending_count, 0) AS local_pending_count,
			COALESCE(progress.local_approved_count, 0) AS local_approved_count,
			COALESCE(progress.local_failed_count, 0) AS local_failed_count,
			COALESCE(progress.local_pending_duration, 0) AS local_pending_duration,
			COALESCE(progress.local_approved_duration, 0) AS local_approved_duration,
			COALESCE(progress.local_failed_duration, 0) AS local_failed_duration,
			(
				SELECT COUNT(*)
				FROM tasks t
				WHERE t.dc_plan_id = dp.id
					AND t.status IN ('ready', 'in_progress', 'uploading')
					AND t.deleted_at IS NULL
					AND NOT EXISTS (
						SELECT 1
						FROM episodes e
						WHERE e.task_id = t.id AND e.deleted_at IS NULL
					)
			) AS reserved_count
		FROM dc_plan dp
		LEFT JOIN (
			SELECT
				e.dc_plan_id,
				SUM(CASE
					WHEN COALESCE(e.qa_status, 'pending_qa') IN ('pending_qa', 'qa_running') THEN 1
					ELSE 0
				END) AS local_pending_count,
				SUM(CASE WHEN e.qa_status = 'approved' THEN 1 ELSE 0 END) AS local_approved_count,
				SUM(CASE
					WHEN e.qa_status IN ('failed', 'manual_review_failed') THEN 1
					ELSE 0
				END) AS local_failed_count,
				SUM(CASE
					WHEN COALESCE(e.qa_status, 'pending_qa') IN ('pending_qa', 'qa_running')
						THEN COALESCE(e.duration_sec, 0)
					ELSE 0
				END) AS local_pending_duration,
				SUM(CASE
					WHEN e.qa_status = 'approved' THEN COALESCE(e.duration_sec, 0)
					ELSE 0
				END) AS local_approved_duration,
				SUM(CASE
					WHEN e.qa_status IN ('failed', 'manual_review_failed')
						THEN COALESCE(e.duration_sec, 0)
					ELSE 0
				END) AS local_failed_duration
				FROM episodes e
				WHERE e.dc_plan_id IS NOT NULL
					AND COALESCE(e.cloud_synced, FALSE) = FALSE
					AND e.deleted_at IS NULL
				GROUP BY e.dc_plan_id
		) progress ON progress.dc_plan_id = dp.id
		WHERE dp.workspace_id = ?
			AND dp.operator = ?
			AND (dp.dc_device_id = ? OR dp.dc_device_id IS NULL)
			AND COALESCE(dp.status, '') <> 'collected'
			AND dp.deleted_at IS NULL
		ORDER BY dp.id
	`, dcDeviceID, claims.WorkspaceID, strings.TrimSpace(claims.OperatorID), dcDeviceID); err != nil {
		logger.Printf("[DC_PLAN] Failed to list operator plans: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to refresh operator plans"})
		return
	}

	response := OperatorPlanRefreshResponse{Items: []OperatorPlanItem{}, Stale: stale}
	var latestSync time.Time
	for _, row := range rows {
		localCount := row.LocalPendingCount + row.LocalApprovedCount
		curCount := row.CurCount + localCount
		pendingDuration := int64(math.Round(row.LocalPendingDuration))
		approvedDuration := int64(math.Round(row.LocalApprovedDuration))
		failedDuration := int64(math.Round(row.LocalFailedDuration))
		localDuration := pendingDuration + approvedDuration
		curDuration := row.CurDuration + localDuration
		reservedCount := curCount + row.ReservedCount
		remaining := row.TargetCount - reservedCount
		if remaining < 0 {
			remaining = 0
		}
		item := OperatorPlanItem{
			ID:                    row.ID,
			WorkspaceID:           row.WorkspaceID,
			Name:                  row.Name,
			DCProjectID:           row.DCProjectID,
			DCProjectName:         row.DCProjectName,
			DCProjectDescription:  row.DCProjectDescription,
			DCTaskID:              row.DCTaskID,
			DCTaskName:            row.DCTaskName,
			DCTaskDescription:     row.DCTaskDescription,
			DCDeviceID:            row.DCDeviceID,
			DCDeviceName:          row.DCDeviceName,
			DCType:                row.DCType,
			TargetCount:           row.TargetCount,
			CurCount:              curCount,
			CloudCurCount:         row.CurCount,
			LocalCurCount:         localCount,
			LocalPendingCount:     row.LocalPendingCount,
			LocalApprovedCount:    row.LocalApprovedCount,
			LocalFailedCount:      row.LocalFailedCount,
			TargetDuration:        row.TargetDuration,
			CurDuration:           curDuration,
			CloudCurDuration:      row.CurDuration,
			LocalCurDuration:      localDuration,
			LocalPendingDuration:  pendingDuration,
			LocalApprovedDuration: approvedDuration,
			LocalFailedDuration:   failedDuration,
			CommittedCount:        reservedCount,
			RemainingCount:        remaining,
		}
		if row.LastSyncedAt.Valid {
			item.LastSyncedAt = row.LastSyncedAt.Time.UTC().Format(time.RFC3339)
			if row.LastSyncedAt.Time.After(latestSync) {
				latestSync = row.LastSyncedAt.Time
			}
		}
		response.Items = append(response.Items, item)
	}
	if !latestSync.IsZero() {
		response.LastSyncedAt = latestSync.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, response)
}

// RegisterAdminRoutes registers dc_plan admin-only routes.
func (h *DCPlanHandler) RegisterAdminRoutes(apiV1 *gin.RouterGroup) {
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
// @Param        dc_project_id   query string false "Comma-separated project IDs"
// @Param        dc_project_name query string false "Fuzzy project name filter"
// @Param        dc_task_id      query string false "Comma-separated task IDs"
// @Param        dc_task_name    query string false "Fuzzy task name filter"
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

	claims := middleware.GetClaims(c)
	if claims != nil && claims.Role == "data_collector" {
		workspaceIDs, err := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, claims.OperatorID)
		if err != nil {
			logger.Printf("[DC_PLAN] Failed to resolve collector Workspace access: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
			return
		}
		if !int64SliceContains(workspaceIDs, workspaceID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "workspace access denied"})
			return
		}
	}

	name := strings.TrimSpace(c.Query("name"))
	dcProjectIDs, err := parseNonNegativeInt64List(c.Query("dc_project_id"), "dc_project_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dcProjectName := strings.TrimSpace(c.Query("dc_project_name"))
	dcTaskIDs, err := parseNonNegativeInt64List(c.Query("dc_task_id"), "dc_task_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	dcTaskName := strings.TrimSpace(c.Query("dc_task_name"))
	dcType := strings.TrimSpace(c.Query("dc_type"))
	operator := strings.TrimSpace(c.Query("operator"))
	if claims != nil && claims.Role == "data_collector" {
		operator = claims.OperatorID
	}
	dcDate := strings.TrimSpace(c.Query("dc_date"))
	if dcDate != "" {
		if parsed, err := time.Parse("2006-01-02", dcDate); err != nil || parsed.Format("2006-01-02") != dcDate {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dc_date format"})
			return
		}
	}

	whereClause := "WHERE dp.deleted_at IS NULL AND dp.workspace_id = ?"
	args := []any{workspaceID}
	whereClause, args = appendKeywordSearch(whereClause, args, name, "dp.name")
	whereClause, args = appendInt64InFilter(whereClause, args, "dp.dc_project_id", dcProjectIDs)
	whereClause, args = appendKeywordSearch(whereClause, args, dcProjectName, "dp.dc_project_name")
	whereClause, args = appendInt64InFilter(whereClause, args, "dp.dc_task_id", dcTaskIDs)
	whereClause, args = appendKeywordSearch(whereClause, args, dcTaskName, "dp.dc_task_name")
	if dcType != "" {
		whereClause += " AND dp.dc_type = ?"
		args = append(args, dcType)
	}
	if operator != "" {
		whereClause += " AND dp.operator = ?"
		args = append(args, operator)
	}
	if dcDate != "" {
		whereClause += " AND dp.dc_date = ?"
		args = append(args, dcDate)
	}

	var total int
	countStartedAt := time.Now()
	if err := h.db.Get(&total, "SELECT COUNT(*) FROM dc_plan dp "+whereClause, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to count dc plans: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
		return
	}
	countDuration := time.Since(countStartedAt)

	query := `
		SELECT
			dp.id, dp.workspace_id, dp.name, dp.description, dp.dc_factory_id, dp.dc_service_provider_id,
			dp.operator, dp.operator_display_name, dp.dc_project_id, dp.dc_project_name, dp.dc_project_description, dp.dc_task_id, dp.dc_task_name, dp.dc_task_description, dp.dc_device_id, dp.dc_device_name, dp.dc_type, CAST(dp.dc_date AS CHAR) AS dc_date,
			dp.target_count, dp.cur_count, dp.target_duration, dp.cur_duration,
			dp.created_by, dp.created_time, dp.updated_by, dp.updated_time, dp.last_synced_at
		FROM dc_plan dp
		` + whereClause + `
		ORDER BY dp.dc_date DESC, dp.id DESC
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var rows []dcPlanRow
	pageStartedAt := time.Now()
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[DC_PLAN] Failed to query dc plans: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
		return
	}
	pageDuration := time.Since(pageStartedAt)

	progressDuration := time.Duration(0)
	if len(rows) > 0 {
		planIDs := make([]int64, 0, len(rows))
		for _, row := range rows {
			planIDs = append(planIDs, row.ID)
		}

		progressStartedAt := time.Now()
		progressByPlanID, err := h.listDCPlanLocalProgress(c, planIDs)
		if err != nil {
			logger.Printf("[DC_PLAN] Failed to query local plan progress: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dc plans"})
			return
		}
		progressDuration = time.Since(progressStartedAt)
		for index := range rows {
			if progress, ok := progressByPlanID[rows[index].ID]; ok {
				rows[index].LocalCurCount = progress.LocalCurCount
				rows[index].LocalCurDuration = sql.NullFloat64{Float64: progress.LocalCurDuration, Valid: true}
			}
		}
	}

	items := make([]DCPlanResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, dcPlanResponseFromRow(row))
	}

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		logger.Printf(
			"[DC_PLAN] Slow list query: workspace_id=%d total=%s count=%s page=%s progress=%s plans=%d",
			workspaceID, elapsed, countDuration, pageDuration, progressDuration, len(rows),
		)
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
		WorkspaceID:  result.WorkspaceID,
		SyncedCount:  result.SyncedCount,
		PageCount:    result.PageCount,
		LastSyncedAt: result.LastSyncedAt.UTC().Format(time.RFC3339),
	})
}

type dcPlanLocalProgressRow struct {
	DCPlanID         int64   `db:"dc_plan_id"`
	LocalCurCount    int64   `db:"local_cur_count"`
	LocalCurDuration float64 `db:"local_cur_duration"`
}

func (h *DCPlanHandler) listDCPlanLocalProgress(c *gin.Context, planIDs []int64) (map[int64]dcPlanLocalProgressRow, error) {
	query, args, err := sqlx.In(`
		SELECT
			dc_plan_id,
			COUNT(*) AS local_cur_count,
			COALESCE(SUM(COALESCE(duration_sec, 0)), 0) AS local_cur_duration
		FROM episodes
		WHERE dc_plan_id IN (?)
			AND deleted_at IS NULL
			AND COALESCE(cloud_synced, FALSE) = FALSE
			AND COALESCE(qa_status, 'pending_qa') NOT IN ('failed', 'manual_review_failed')
		GROUP BY dc_plan_id
	`, planIDs)
	if err != nil {
		return nil, err
	}

	var rows []dcPlanLocalProgressRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, h.db.Rebind(query), args...); err != nil {
		return nil, err
	}

	progressByPlanID := make(map[int64]dcPlanLocalProgressRow, len(rows))
	for _, row := range rows {
		progressByPlanID[row.DCPlanID] = row
	}
	return progressByPlanID, nil
}

func parseRequiredNonNegativeQueryInt64(c *gin.Context, field string) (int64, bool) {
	raw := strings.TrimSpace(c.Query(field))
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": field + " is required"})
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": field + " must be a non-negative integer"})
		return 0, false
	}
	return value, true
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
	curCount := row.CurCount + row.LocalCurCount
	curDuration := row.CurDuration
	if row.LocalCurDuration.Valid {
		curDuration += int64(math.Round(row.LocalCurDuration.Float64))
	}
	var dcDeviceID int64
	if row.DCDeviceID.Valid {
		dcDeviceID = row.DCDeviceID.Int64
	}
	return DCPlanResponse{
		ID:                   row.ID,
		WorkspaceID:          row.WorkspaceID,
		Name:                 row.Name,
		Description:          row.Description.String,
		DCFactoryID:          row.DCFactoryID,
		DCServiceProviderID:  row.DCServiceProviderID,
		Operator:             row.Operator,
		OperatorDisplayName:  row.OperatorDisplayName.String,
		DCProjectID:          row.DCProjectID,
		DCProjectName:        row.DCProjectName.String,
		DCProjectDescription: row.DCProjectDescription.String,
		DCTaskID:             row.DCTaskID,
		DCTaskName:           row.DCTaskName.String,
		DCTaskDescription:    row.DCTaskDescription.String,
		DCDeviceID:           dcDeviceID,
		DCDeviceName:         row.DCDeviceName.String,
		DCType:               row.DCType,
		DCDate:               row.DCDate,
		TargetCount:          row.TargetCount,
		CurCount:             curCount,
		TargetDuration:       row.TargetDuration,
		CurDuration:          curDuration,
		CreatedBy:            row.CreatedBy,
		CreatedTime:          formatWorkspaceNullableTime(row.CreatedTime),
		UpdatedBy:            row.UpdatedBy.String,
		UpdatedTime:          formatWorkspaceNullableTime(row.UpdatedTime),
		LastSyncedAt:         formatWorkspaceNullableTime(row.LastSyncedAt),
	}
}

func int64SliceContains(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
