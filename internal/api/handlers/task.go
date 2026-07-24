// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
	"archebase.com/keystone-edge/internal/services"
)

type transferUploadHub interface {
	Get(deviceID string) *services.TransferConn
	SendToDeviceWithTimeout(ctx context.Context, deviceID string, msg map[string]interface{}, timeout time.Duration) error
}

// TaskHandler handles task-related HTTP requests
type TaskHandler struct {
	db                   *sqlx.DB
	hub                  transferUploadHub
	recorderHub          *services.RecorderHub
	recorderRPCTimeout   time.Duration
	transferWriteTimeout time.Duration
	callbackURLs         callbackURLs
	taskSupply           *services.DCPlanTaskSupplyService
}

// NewTaskHandler creates a new TaskHandler.
func NewTaskHandler(db *sqlx.DB, hub *services.TransferHub, recorderHub *services.RecorderHub, recorderRPCTimeout time.Duration, transferWriteTimeout ...time.Duration) *TaskHandler {
	writeTimeout := services.DefaultTransferWriteTimeout
	if len(transferWriteTimeout) > 0 && transferWriteTimeout[0] > 0 {
		writeTimeout = transferWriteTimeout[0]
	}
	return &TaskHandler{
		db:                   db,
		hub:                  hub,
		recorderHub:          recorderHub,
		recorderRPCTimeout:   recorderRPCTimeout,
		transferWriteTimeout: writeTimeout,
		taskSupply:           services.NewDCPlanTaskSupplyService(db),
	}
}

// SetCallbackPublicBaseURL configures Keystone callback URLs returned in task configs.
func (h *TaskHandler) SetCallbackPublicBaseURL(callbackPublicBaseURL string) {
	if h == nil {
		return
	}
	h.callbackURLs = newCallbackURLs(callbackPublicBaseURL)
}

func (h *TaskHandler) axonTransferWriteTimeout() time.Duration {
	if h == nil || h.transferWriteTimeout <= 0 {
		return services.DefaultTransferWriteTimeout
	}
	return h.transferWriteTimeout
}

// TaskConfig represents the task configuration response
type TaskConfig struct {
	TaskID               string   `json:"task_id"`
	DeviceID             string   `json:"device_id"`
	DataCollectorID      string   `json:"data_collector_id"`
	WorkstationID        string   `json:"workstation_id"`
	OperatorName         string   `json:"operator_name,omitempty"`
	Topics               []string `json:"topics"`
	StartCallbackURL     string   `json:"start_callback_url"`
	FinishCallbackURL    string   `json:"finish_callback_url"`
	UserToken            string   `json:"user_token"`
	WorkspaceID          *int64   `json:"workspace_id,omitempty"`
	DCPlanID             *int64   `json:"dc_plan_id,omitempty"`
	DCPlanName           string   `json:"dc_plan_name,omitempty"`
	DCProjectDescription string   `json:"dc_project_description,omitempty"`
	DCTaskDescription    string   `json:"dc_task_description,omitempty"`
	DCType               string   `json:"dc_type,omitempty"`
	DCDeviceID           *int64   `json:"dc_device_id,omitempty"`
	PlanTargetCount      *int64   `json:"plan_target_count,omitempty"`
	PlanTargetDuration   *int64   `json:"plan_target_duration,omitempty"`
}

// RegisterRoutes registers task-related routes
func (h *TaskHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/tasks", h.ListTasks)
	apiV1.GET("/tasks/:id", h.GetTask)
	apiV1.DELETE("/tasks/:id", h.DeleteTask)
	apiV1.GET("/tasks/:id/config", h.GetTaskConfig)
}

// RegisterCollectorRoutes registers task operations restricted to data collectors.
func (h *TaskHandler) RegisterCollectorRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/tasks/complete", h.CompleteTasks)
	apiV1.POST("/tasks/:id/capture/start", h.StartCollectorCapture)
	apiV1.POST("/tasks/:id/capture/finish", h.FinishCollectorCapture)
	apiV1.POST("/tasks/:id/capture/abandon", h.AbandonCollectorCapture)
	apiV1.POST("/dc-plans/:id/tasks/next", h.EnsureNextPlanTask)
}

// CollectorCaptureStateResponse reports the server-side task state for an EgoPortal capture.
type CollectorCaptureStateResponse struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// StartCollectorCapture marks the authenticated workstation task as actively recording.
//
//	@Summary	Start EgoPortal task capture
//	@Tags		tasks
//	@Produce	json
//	@Param		id	path		int	true	"Numeric task ID"
//	@Success	200	{object}	CollectorCaptureStateResponse
//	@Failure	400	{object}	map[string]any
//	@Failure	401	{object}	map[string]any
//	@Failure	404	{object}	map[string]any
//	@Failure	409	{object}	map[string]any
//	@Failure	500	{object}	map[string]any
//	@Router		/tasks/{id}/capture/start [post]
func (h *TaskHandler) StartCollectorCapture(c *gin.Context) {
	h.transitionCollectorCapture(c, "start")
}

// FinishCollectorCapture marks local recording complete and ready for device-authenticated upload.
//
//	@Summary	Finish EgoPortal task capture
//	@Tags		tasks
//	@Produce	json
//	@Param		id	path		int	true	"Numeric task ID"
//	@Success	200	{object}	CollectorCaptureStateResponse
//	@Failure	400	{object}	map[string]any
//	@Failure	401	{object}	map[string]any
//	@Failure	404	{object}	map[string]any
//	@Failure	409	{object}	map[string]any
//	@Failure	500	{object}	map[string]any
//	@Router		/tasks/{id}/capture/finish [post]
func (h *TaskHandler) FinishCollectorCapture(c *gin.Context) {
	h.transitionCollectorCapture(c, "finish")
}

// AbandonCollectorCapture releases an unfinished EgoPortal capture task before its local data is discarded.
//
//	@Summary	Abandon EgoPortal task capture
//	@Tags		tasks
//	@Produce	json
//	@Param		id	path		int	true	"Numeric task ID"
//	@Success	200	{object}	CollectorCaptureStateResponse
//	@Failure	400	{object}	map[string]any
//	@Failure	401	{object}	map[string]any
//	@Failure	404	{object}	map[string]any
//	@Failure	409	{object}	map[string]any
//	@Failure	500	{object}	map[string]any
//	@Router		/tasks/{id}/capture/abandon [post]
func (h *TaskHandler) AbandonCollectorCapture(c *gin.Context) {
	taskID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || taskID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" || claims.WorkstationID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "workstation session is required"})
		return
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), nil)
	if err != nil {
		logger.Printf("[TASK] Collector capture abandon transaction failed: task=%d err=%v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abandon task capture"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var task struct {
		ID     int64  `db:"id"`
		TaskID string `db:"task_id"`
		Status string `db:"status"`
	}
	taskQuery := `
		SELECT id, task_id, status FROM tasks
		WHERE id = ? AND workstation_id = ? AND deleted_at IS NULL
	`
	if tx.DriverName() != "sqlite" {
		taskQuery += " FOR UPDATE"
	}
	if err := tx.GetContext(c.Request.Context(), &task, taskQuery, taskID, claims.WorkstationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		logger.Printf("[TASK] Collector capture abandon task lookup failed: task=%d err=%v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query task"})
		return
	}

	var hasEpisode bool
	if err := tx.GetContext(c.Request.Context(), &hasEpisode, `
		SELECT EXISTS(
			SELECT 1 FROM episodes
			WHERE task_id = ? AND deleted_at IS NULL
		)
	`, task.ID); err != nil {
		logger.Printf("[TASK] Collector capture abandon episode lookup failed: task=%d err=%v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abandon task capture"})
		return
	}
	if hasEpisode {
		c.JSON(http.StatusConflict, gin.H{
			"code":           "task_capture_has_episode",
			"current_status": task.Status,
			"error":          "uploaded task capture cannot be abandoned",
		})
		return
	}

	switch task.Status {
	case "cancelled", "failed":
	case "pending", "ready", "in_progress", "uploading":
		now := time.Now().UTC()
		// #nosec G701 -- static SQL with placeholder-bound timestamp and task ID.
		if _, err := tx.ExecContext(c.Request.Context(), `
			UPDATE tasks
			SET status = 'cancelled', error_message = 'collector_abandoned_local_capture', updated_at = ?
			WHERE id = ? AND deleted_at IS NULL
		`, now, task.ID); err != nil {
			logger.Printf("[TASK] Collector capture abandon failed: task=%d err=%v", taskID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abandon task capture"})
			return
		}
		task.Status = "cancelled"
	default:
		c.JSON(http.StatusConflict, gin.H{
			"code":           "task_capture_state_conflict",
			"current_status": task.Status,
			"error":          "task capture cannot be abandoned",
		})
		return
	}
	if err := tx.Commit(); err != nil {
		logger.Printf("[TASK] Collector capture abandon commit failed: task=%d err=%v", taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abandon task capture"})
		return
	}
	c.JSON(http.StatusOK, CollectorCaptureStateResponse{
		ID:     strconv.FormatInt(task.ID, 10),
		TaskID: task.TaskID,
		Status: task.Status,
	})
}

func (h *TaskHandler) transitionCollectorCapture(c *gin.Context, action string) {
	taskID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || taskID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" || claims.WorkstationID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "workstation session is required"})
		return
	}

	fromStatuses := []string{"pending", "ready"}
	targetStatus := "in_progress"
	if action == "finish" {
		fromStatuses = []string{"in_progress"}
		targetStatus = "uploading"
	}
	query, args, err := sqlx.In(`
		UPDATE tasks
		SET status = ?,
			started_at = CASE WHEN ? = 'in_progress' THEN COALESCE(started_at, ?) ELSE started_at END,
			updated_at = ?
		WHERE id = ? AND workstation_id = ? AND status IN (?) AND deleted_at IS NULL
	`, targetStatus, targetStatus, time.Now().UTC(), time.Now().UTC(), taskID, claims.WorkstationID, fromStatuses)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update task"})
		return
	}
	// #nosec G701 -- sqlx.In only expands placeholders; all values remain bound after Rebind.
	result, err := h.db.ExecContext(c.Request.Context(), h.db.Rebind(query), args...)
	if err != nil {
		logger.Printf("[TASK] Collector capture %s failed: task=%d err=%v", action, taskID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update task"})
		return
	}
	affected, _ := result.RowsAffected()

	var task struct {
		ID     int64  `db:"id"`
		TaskID string `db:"task_id"`
		Status string `db:"status"`
	}
	if err := h.db.GetContext(c.Request.Context(), &task, `
		SELECT id, task_id, status FROM tasks
		WHERE id = ? AND workstation_id = ? AND deleted_at IS NULL
	`, taskID, claims.WorkstationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query task"})
		return
	}
	if affected == 0 {
		if (action == "start" && task.Status != "in_progress") ||
			(action == "finish" && task.Status != "uploading" && task.Status != "completed") {
			c.JSON(http.StatusConflict, gin.H{
				"code":           "task_capture_state_conflict",
				"current_status": task.Status,
				"error":          "task capture state cannot be changed",
			})
			return
		}
	}
	c.JSON(http.StatusOK, CollectorCaptureStateResponse{
		ID:     strconv.FormatInt(task.ID, 10),
		TaskID: task.TaskID,
		Status: task.Status,
	})
}

// EnsureNextPlanTask creates or reuses the pending task for a collector plan.
//
// @Summary      Ensure next plan task
// @Description  Creates or reuses the single pending task for the authenticated collector workstation.
// @Tags         tasks
// @Produce      json
// @Param        id path int true "DC plan ID"
// @Success      200 {object} services.DCPlanTaskSupplyResult
// @Failure      400 {object} map[string]string
// @Failure      403 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /dc-plans/{id}/tasks/next [post]
func (h *TaskHandler) EnsureNextPlanTask(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" || claims.WorkstationID <= 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "data collector workstation required"})
		return
	}

	planID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || planID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dc plan id"})
		return
	}

	result, err := h.taskSupply.EnsureNextTask(
		c.Request.Context(), planID, claims.WorkstationID, time.Now().UTC(),
	)
	if err == nil {
		c.JSON(http.StatusOK, result)
		return
	}

	switch {
	case errors.Is(err, services.ErrDCPlanTaskSupplyNotFound):
		c.JSON(http.StatusNotFound, gin.H{"code": "plan_not_found", "error": "dc plan not found"})
	case errors.Is(err, services.ErrDCPlanTaskSupplyWorkstationMismatch):
		c.JSON(http.StatusForbidden, gin.H{
			"code": "plan_workstation_mismatch", "error": "dc plan is not assigned to this workstation",
		})
	case errors.Is(err, services.ErrDCPlanTaskSupplyTargetReached):
		c.JSON(http.StatusConflict, gin.H{
			"code": "plan_target_reached", "error": "dc plan target has been reached",
		})
	case errors.Is(err, services.ErrDCPlanTaskSupplyActiveTask):
		c.JSON(http.StatusConflict, gin.H{
			"code": "task_already_active", "error": "another task is already active",
		})
	default:
		logger.Printf("[TASK] Ensure next plan task failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to ensure next task"})
	}
}

// CompleteTasksRequest identifies pending tasks to complete for the current workstation.
type CompleteTasksRequest struct {
	DCPlanID int64 `json:"dc_plan_id" binding:"required"`
	Quantity int   `json:"quantity" binding:"required"`
}

// CompletedTask describes one task completed by an external-device workflow.
type CompletedTask struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	CompletedAt string `json:"completed_at"`
}

// CompleteTasksResponse reports tasks completed for one DC plan group.
type CompleteTasksResponse struct {
	DCPlanID       int64           `json:"dc_plan_id"`
	RequestedCount int             `json:"requested_count"`
	CompletedCount int             `json:"completed_count"`
	Tasks          []CompletedTask `json:"tasks"`
}

var validTaskStatuses = map[string]struct{}{
	"pending":     {},
	"ready":       {},
	"in_progress": {},
	"uploading":   {},
	"completed":   {},
	"failed":      {},
	"cancelled":   {},
}

// TaskListItem represents a task item in list responses.
type TaskListItem struct {
	ID                   int64          `json:"id" db:"id"`
	TaskID               string         `json:"task_id" db:"task_id"`
	WorkstationID        *string        `json:"workstation_id" db:"workstation_id"`
	RobotDeviceID        *string        `json:"robot_device_id" db:"robot_device_id"`
	CollectorOperatorID  *string        `json:"collector_operator_id" db:"collector_operator_id"`
	Status               string         `json:"status" db:"status"`
	ErrorMessage         *string        `json:"error_message" db:"error_message"`
	AssignedAt           *string        `json:"assigned_at" db:"assigned_at"`
	DCPlanID             *int64         `json:"dc_plan_id,omitempty" db:"dc_plan_id"`
	WorkspaceID          *int64         `json:"workspace_id,omitempty" db:"workspace_id"`
	DCPlanName           *string        `json:"dc_plan_name,omitempty" db:"dc_plan_name"`
	DCProjectDescription string         `json:"dc_project_description,omitempty"`
	DCTaskDescription    string         `json:"dc_task_description,omitempty"`
	DCType               *string        `json:"dc_type,omitempty" db:"dc_type"`
	DCDeviceID           *int64         `json:"dc_device_id,omitempty" db:"dc_device_id"`
	PlanSnapshotRaw      sql.NullString `json:"-" db:"plan_snapshot_raw"`
}

// ListTasksResponse represents the response body for listing tasks.
type ListTasksResponse struct {
	Items   []TaskListItem `json:"items"`
	Total   int            `json:"total"`
	Limit   int            `json:"limit"`
	Offset  int            `json:"offset"`
	HasNext bool           `json:"hasNext,omitempty"`
	HasPrev bool           `json:"hasPrev,omitempty"`
}

// TaskEpisodeDetail represents the episode information attached to a task.
// Shape matches GET /api/v1/episodes/:id (Episode): id is episodes.id (PK), episode_id is the human-readable column.
type TaskEpisodeDetail struct {
	ID        int64  `json:"id"`
	EpisodeID string `json:"episode_id,omitempty"`
}

// TaskDetailResponse represents the response body for getting a task by ID.
type TaskDetailResponse struct {
	ID               int64              `json:"id" db:"id"`
	TaskID           string             `json:"task_id" db:"task_id"`
	WorkstationID    *string            `json:"workstation_id" db:"workstation_id"`
	OrganizationID   *int64             `json:"organization_id" db:"organization_id"`
	DCPlanID         *int64             `json:"dc_plan_id,omitempty" db:"dc_plan_id"`
	WorkspaceID      *int64             `json:"workspace_id,omitempty" db:"workspace_id"`
	DCPlanName       *string            `json:"dc_plan_name,omitempty" db:"dc_plan_name"`
	DCType           *string            `json:"dc_type,omitempty" db:"dc_type"`
	DCDeviceID       *int64             `json:"dc_device_id,omitempty" db:"dc_device_id"`
	Status           string             `json:"status" db:"status"`
	ErrorMessage     *string            `json:"error_message" db:"error_message"`
	CreatedAt        *string            `json:"created_at" db:"created_at"`
	AssignedAt       *string            `json:"assigned_at" db:"assigned_at"`
	StartedAt        *string            `json:"started_at" db:"started_at"`
	CompletedAt      *string            `json:"completed_at" db:"completed_at"`
	Episode          *TaskEpisodeDetail `json:"episode"`
	EpisodeNumericID sql.NullInt64      `db:"episode_numeric_id" json:"-"`
	EpisodePublicID  sql.NullString     `db:"episode_public_id" json:"-"`
}

// ListTasks handles task listing requests with optional filtering.
//
// @Summary      List tasks
// @Description  Lists tasks with optional workstation and status filters
// @Tags         tasks
// @Produce      json
// @Param        task_id         query     string  false  "Filter by public task_id"
// @Param        workstation_id  query     string  false  "Filter by workstation"
// @Param        status          query     string  false  "Filter by status"
// @Param        limit           query     int     false  "Max results"      default(50)
// @Param        offset          query     int     false  "Pagination offset" default(0)
// @Success      200             {object}  ListTasksResponse
// @Failure      400             {object}  map[string]string
// @Failure      500             {object}  map[string]string
// @Router       /tasks [get]
func (h *TaskHandler) ListTasks(c *gin.Context) {
	const defaultLimit = 50

	taskID := strings.TrimSpace(c.Query("task_id"))
	workstationID := strings.TrimSpace(c.Query("workstation_id"))
	status := strings.TrimSpace(c.Query("status"))
	workspaceID := strings.TrimSpace(c.Query("workspace_id"))
	dcPlanID := strings.TrimSpace(c.Query("dc_plan_id"))

	limit := defaultLimit
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = parsedLimit
	}

	offset := 0
	if rawOffset := strings.TrimSpace(c.Query("offset")); rawOffset != "" {
		parsedOffset, err := strconv.Atoi(rawOffset)
		if err != nil || parsedOffset < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "offset must be a non-negative integer"})
			return
		}
		offset = parsedOffset
	}

	if status != "" {
		if _, ok := validTaskStatuses[status]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
	}
	if workspaceID != "" {
		parsed, err := strconv.ParseInt(workspaceID, 10, 64)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace_id format"})
			return
		}
	}
	if dcPlanID != "" {
		parsed, err := strconv.ParseInt(dcPlanID, 10, 64)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dc_plan_id format"})
			return
		}
	}
	claims := middleware.GetClaims(c)

	conditions := []string{"tasks.deleted_at IS NULL"}
	args := make([]interface{}, 0, 6)

	if taskID != "" {
		conditions = append(conditions, "tasks.task_id = ?")
		args = append(args, taskID)
	}

	if workstationID != "" {
		conditions = append(conditions, "CAST(tasks.workstation_id AS CHAR) = ?")
		args = append(args, workstationID)
	}

	if status != "" {
		conditions = append(conditions, "tasks.status = ?")
		args = append(args, status)
	}
	if workspaceID != "" {
		conditions = append(conditions, "COALESCE(tasks.organization_id, ws.workspace_id) = ?")
		args = append(args, workspaceID)
	}
	if dcPlanID != "" {
		conditions = append(conditions, "tasks.dc_plan_id = ?")
		args = append(args, dcPlanID)
	}
	if claims != nil && claims.Role == "data_collector" {
		workspaceIDs, accessErr := services.AccessibleWorkspaceIDs(c.Request.Context(), h.db, claims.OperatorID)
		if accessErr != nil {
			logger.Printf("[TASK] Failed to resolve collector Workspace access: %v", accessErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
			return
		}
		if len(workspaceIDs) == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "workspace access denied"})
			return
		}
		placeholders := make([]string, 0, len(workspaceIDs))
		conditions = append(conditions, "ws.data_collector_id = ?")
		args = append(args, claims.CollectorID)
		for _, id := range workspaceIDs {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		conditions = append(conditions, "COALESCE(tasks.organization_id, ws.workspace_id) IN ("+strings.Join(placeholders, ",")+")")
	}

	whereClause := strings.Join(conditions, " AND ")

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM tasks LEFT JOIN workstations ws ON ws.id = tasks.workstation_id AND ws.deleted_at IS NULL LEFT JOIN dc_plan dp ON dp.id = tasks.dc_plan_id AND dp.deleted_at IS NULL WHERE %s", whereClause)
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[TASK] Failed to count tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}

	queryArgs := append(append([]interface{}{}, args...), limit, offset)
	listQuery := fmt.Sprintf(`SELECT
		tasks.id AS id,
		tasks.task_id AS task_id,
		CASE WHEN tasks.workstation_id IS NULL THEN NULL ELSE CAST(tasks.workstation_id AS CHAR) END AS workstation_id,
		NULLIF(TRIM(COALESCE(ws.robot_serial, '')), '') AS robot_device_id,
		NULLIF(TRIM(COALESCE(ws.collector_operator_id, '')), '') AS collector_operator_id,
		tasks.status,
		tasks.error_message,
		tasks.metadata AS plan_snapshot_raw,
		CASE WHEN tasks.assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(tasks.assigned_at, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') END AS assigned_at,
		tasks.dc_plan_id AS dc_plan_id,
		COALESCE(tasks.organization_id, ws.workspace_id) AS workspace_id,
		dp.name AS dc_plan_name,
		dp.dc_type AS dc_type,
		dp.dc_device_id AS dc_device_id
		FROM tasks
		LEFT JOIN workstations ws ON ws.id = tasks.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN dc_plan dp ON dp.id = tasks.dc_plan_id AND dp.deleted_at IS NULL
		WHERE %s
		ORDER BY tasks.created_at DESC, tasks.id DESC
		LIMIT ? OFFSET ?`, whereClause)

	items := make([]TaskListItem, 0)
	if err := h.db.Select(&items, listQuery, queryArgs...); err != nil {
		logger.Printf("[TASK] Failed to query tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}
	for index := range items {
		applyTaskPlanSnapshot(&items[index], items[index].PlanSnapshotRaw.String)
	}

	hasNext := (offset + limit) < total
	hasPrev := offset > 0

	c.JSON(http.StatusOK, ListTasksResponse{
		Items:   items,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// CompleteTasks marks pending tasks in one DC plan group as completed.
//
// @Summary      Complete DC plan tasks
// @Description  Completes pending tasks assigned to the current data collector workstation.
// @Tags         tasks
// @Accept       json
// @Produce      json
// @Param        body body CompleteTasksRequest true "DC plan task group and quantity"
// @Success      200 {object} CompleteTasksResponse
// @Failure      400 {object} map[string]interface{}
// @Failure      401 {object} map[string]string
// @Failure      403 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /tasks/complete [post]
func (h *TaskHandler) CompleteTasks(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	if claims.Role != "data_collector" {
		c.JSON(http.StatusForbidden, gin.H{"error": "data collector role required"})
		return
	}

	var req CompleteTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.DCPlanID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task group"})
		return
	}
	if req.Quantity < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quantity must be >= 1"})
		return
	}

	now := time.Now().UTC()
	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[TASK] complete tasks begin transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	var planWorkspaceID int64
	if err := tx.Get(&planWorkspaceID, `
		SELECT workspace_id
		FROM dc_plan
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, req.DCPlanID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "dc plan not found"})
			return
		}
		logger.Printf("[TASK] complete tasks plan query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}
	allowed, err := services.OperatorHasWorkspaceAccess(c.Request.Context(), tx, claims.OperatorID, planWorkspaceID)
	if err != nil {
		logger.Printf("[TASK] complete tasks Workspace access query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "workspace access denied"})
		return
	}
	var workstationID int64
	if err := tx.Get(&workstationID, `
		SELECT id
		FROM workstations
		WHERE data_collector_id = ? AND workspace_id = ? AND is_current = TRUE AND deleted_at IS NULL
		LIMIT 1
	`, claims.CollectorID, planWorkspaceID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "workstation not assigned for dc plan workspace"})
			return
		}
		logger.Printf("[TASK] complete tasks workstation query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}

	type pendingTaskRow struct {
		ID     int64  `db:"id"`
		TaskID string `db:"task_id"`
	}
	pendingTasks := make([]pendingTaskRow, 0, req.Quantity)
	lockClause := " FOR UPDATE"
	if tx.DriverName() == "sqlite" {
		lockClause = ""
	}
	if err := tx.Select(&pendingTasks, `
		SELECT id, task_id
		FROM tasks
		WHERE dc_plan_id = ?
			AND workstation_id = ?
			AND status = 'pending'
			AND deleted_at IS NULL
		ORDER BY assigned_at ASC, id ASC
		LIMIT ?`+lockClause, req.DCPlanID, workstationID, req.Quantity); err != nil {
		logger.Printf("[TASK] complete tasks pending query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}
	if len(pendingTasks) < req.Quantity {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         fmt.Sprintf("quantity exceeds pending task count: pending_count=%d, requested=%d", len(pendingTasks), req.Quantity),
			"pending_count": len(pendingTasks),
			"requested":     req.Quantity,
		})
		return
	}

	completedAt := now.Format(time.RFC3339)
	completedTasks := make([]CompletedTask, 0, len(pendingTasks))
	for _, task := range pendingTasks {
		result, err := tx.Exec(`
			UPDATE tasks
			SET status = 'completed',
				started_at = COALESCE(started_at, ?),
				completed_at = ?,
				updated_at = ?
			WHERE id = ? AND status = 'pending' AND deleted_at IS NULL
		`, now, now, now, task.ID)
		if err != nil {
			logger.Printf("[TASK] complete task %d update failed: %v", task.ID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
			return
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			logger.Printf("[TASK] complete task %d rows affected failed: %v", task.ID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
			return
		}
		if rowsAffected == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "task was updated by another request"})
			return
		}
		completedTasks = append(completedTasks, CompletedTask{
			ID:          strconv.FormatInt(task.ID, 10),
			TaskID:      task.TaskID,
			Status:      "completed",
			CompletedAt: completedAt,
		})
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[TASK] complete tasks commit failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to complete tasks"})
		return
	}

	c.JSON(http.StatusOK, CompleteTasksResponse{
		DCPlanID:       req.DCPlanID,
		RequestedCount: req.Quantity,
		CompletedCount: len(completedTasks),
		Tasks:          completedTasks,
	})
}

// GetTask handles task detail requests.
//
// @Summary      Get task detail
// @Description  Returns a task by ID
// @Tags         tasks
// @Produce      json
// @Param        id   path      string  true  "Task ID"
// @Success      200  {object}  TaskDetailResponse
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /tasks/{id} [get]
func (h *TaskHandler) GetTask(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "invalid task id"})
		return
	}
	if !h.authorizeCollectorTask(c, id) {
		return
	}

	var task TaskDetailResponse
	query := `SELECT
		t.id AS id,
		t.task_id AS task_id,
		CASE WHEN t.workstation_id IS NULL THEN NULL ELSE CAST(t.workstation_id AS CHAR) END AS workstation_id,
		t.organization_id AS organization_id,
		t.dc_plan_id AS dc_plan_id,
		COALESCE(t.organization_id, ws.workspace_id) AS workspace_id,
		dp.name AS dc_plan_name,
		dp.dc_type AS dc_type,
		dp.dc_device_id AS dc_device_id,
		t.status,
		t.error_message,
		CASE WHEN t.created_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.created_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS created_at,
		CASE WHEN t.assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.assigned_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS assigned_at,
		CASE WHEN t.started_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.started_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS started_at,
		CASE WHEN t.completed_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.completed_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS completed_at,
		e.id AS episode_numeric_id,
		e.episode_id AS episode_public_id
		FROM tasks t
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		LEFT JOIN episodes e ON e.task_id = t.id AND e.deleted_at IS NULL
		WHERE t.id = ? AND t.deleted_at IS NULL
		LIMIT 1`

	err = h.db.Get(&task, query, id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{
			"error_msg": "Task not found: " + idStr,
		})
		return
	}

	if err != nil {
		logger.Printf("[TASK] Failed to query task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task"})
		return
	}

	if task.EpisodeNumericID.Valid {
		task.Episode = &TaskEpisodeDetail{
			ID:        task.EpisodeNumericID.Int64,
			EpisodeID: task.EpisodePublicID.String,
		}
	}

	c.JSON(http.StatusOK, task)
}

// DeleteTask handles soft deletion of a task.
// Tasks with status "completed" cannot be deleted.
//
// @Summary      Delete task
// @Description  Soft deletes a task. Tasks with status 'completed' cannot be deleted.
// @Tags         tasks
// @Produce      json
// @Param        id path string true "Task ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /tasks/{id} [delete]
func (h *TaskHandler) DeleteTask(c *gin.Context) {
	if claims := middleware.GetClaims(c); claims != nil && claims.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error_msg": "admin role required"})
		return
	}
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "invalid task id"})
		return
	}

	var taskStatus string
	if err := h.db.Get(&taskStatus, "SELECT status FROM tasks WHERE id = ? AND deleted_at IS NULL LIMIT 1", id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error_msg": "task not found"})
			return
		}
		logger.Printf("[TASK] Failed to query task status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to delete task"})
		return
	}

	// Completed/uploading tasks cannot be deleted because their files or audit trail may still be in use.
	if taskStatus == "completed" || taskStatus == "uploading" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": "cannot delete a completed or uploading task"})
		return
	}

	now := time.Now().UTC()
	if _, err := h.db.Exec(
		"UPDATE tasks SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
		now, now, id,
	); err != nil {
		logger.Printf("[TASK] Failed to delete task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to delete task"})
		return
	}

	c.Status(http.StatusNoContent)
}

// RegisterCallbackRoutes registers callback routes for handling external events.
// It sets up POST /start and POST /finish endpoints to handle recording start/finish callbacks.
func (h *TaskHandler) RegisterCallbackRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/start", h.OnRecordingStart)
	apiV1.POST("/finish", h.OnRecordingFinish)
}

// RecordingStartCallback represents the callback payload from axon recorder
type RecordingStartCallback struct {
	TaskID    string   `json:"task_id"`
	DeviceID  string   `json:"device_id"`
	Status    string   `json:"status"`
	StartedAt string   `json:"started_at"`
	Topics    []string `json:"topics"`
}

// RecordingFinishCallback represents the callback payload from axon recorder
type RecordingFinishCallback struct {
	TaskID        string   `json:"task_id"`
	DeviceID      string   `json:"device_id"`
	Status        string   `json:"status"`
	StartedAt     string   `json:"started_at"`
	FinishedAt    string   `json:"finished_at"`
	DurationSec   float64  `json:"duration_sec"`
	MessageCount  int64    `json:"message_count"`
	FileSizeBytes int64    `json:"file_size_bytes"`
	OutputPath    string   `json:"output_path"`
	SidecarPath   string   `json:"sidecar_path"`
	Topics        []string `json:"topics"`
	Metadata      struct {
		Scene    string `json:"scene"`
		Subscene string `json:"subscene"`
		Factory  string `json:"factory"`
	} `json:"metadata"`
	Error string `json:"error"`
}

// OnRecordingStart handles callback from axon recorder when recording starts.
// @Summary      Recording start callback
// @Description  Handles callback from axon recorder when recording starts and acknowledges the callback when task status is ready
// @Tags         callbacks
// @Accept       json
// @Produce      json
// @Param        body  body      RecordingStartCallback  true  "Recording start callback payload"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      404  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Router       /callbacks/start [post]
func (h *TaskHandler) OnRecordingStart(c *gin.Context) {
	var callback RecordingStartCallback
	if err := c.ShouldBindJSON(&callback); err != nil {
		logger.Printf("[RECORDER] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	logger.Printf("%s received start callback", recorderTaskLogPrefix(callback.DeviceID, callback.TaskID))

	// Validate required fields
	if callback.TaskID == "" {
		logger.Printf("%s missing task_id in callback", recorderLogPrefix(callback.DeviceID))
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: task_id",
		})
		return
	}

	taskStatus := "unknown"
	if h.db != nil {
		rowsAffected, previousStatus, err := advanceTaskPendingOrReadyToInProgress(h.db, callback.TaskID)
		if err != nil {
			logger.Printf("%s failed to advance task pending/ready->in_progress after start callback: err=%v", recorderTaskLogPrefix(callback.DeviceID, callback.TaskID), err)
		} else if rowsAffected > 0 {
			taskStatus = "in_progress"
			logger.Printf("%s task status updated: %s -> in_progress reason=start_callback", recorderTaskLogPrefix(callback.DeviceID, callback.TaskID), taskStatusLogValue(previousStatus, "unknown"))
		}
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)
	c.JSON(http.StatusOK, gin.H{
		"status":          "acknowledged",
		"task_status":     taskStatus,
		"acknowledged_at": nowStr,
	})
}

// OnRecordingFinish handles callback from axon recorder when recording finishes.
// @Summary      Recording finish callback
// @Description  Handles callback from axon recorder when recording finishes, triggers upload process if device is connected
// @Tags         callbacks
// @Accept       json
// @Produce      json
// @Param        body  body      RecordingFinishCallback  true  "Recording finish callback payload"
// @Success      200  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]interface{}
// @Failure      409  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]interface{}
// @Router       /callbacks/finish [post]
func (h *TaskHandler) OnRecordingFinish(c *gin.Context) {
	var callback RecordingFinishCallback
	if err := c.ShouldBindJSON(&callback); err != nil {
		logger.Printf("[RECORDER] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	// Validate required fields
	if callback.TaskID == "" {
		logger.Printf("[RECORDER] Failed to parse callback: missing task_id")
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: task_id",
		})
		return
	}

	if callback.OutputPath == "" {
		logger.Printf("%s failed to parse callback: missing output_path", recorderTaskLogPrefix(callback.DeviceID, callback.TaskID))
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: output_path",
		})
		return
	}

	deviceID := callback.DeviceID
	if deviceID == "" {
		logger.Printf("[RECORDER] Failed to parse callback: missing device_id")
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: device_id",
		})
		return
	}

	logger.Printf("%s received finish callback", recorderTaskLogPrefix(callback.DeviceID, callback.TaskID))

	if h.db != nil {
		previousStatus, owned, err := currentOwnedTaskStatus(c.Request.Context(), h.db, deviceID, callback.TaskID)
		if err != nil {
			logger.Printf("%s failed to query task status after finish callback: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task status"})
			return
		}
		if !owned {
			logger.Printf("%s finish callback rejected: task is not owned by device", recorderTaskLogPrefix(deviceID, callback.TaskID))
			c.JSON(http.StatusConflict, gin.H{
				"error_msg": "task is not owned by device or is not uploadable",
			})
			return
		}
		switch previousStatus {
		case "uploading", "completed":
			logger.Printf("%s finish callback idempotent: current_status=%s", recorderTaskLogPrefix(deviceID, callback.TaskID), previousStatus)
			c.JSON(http.StatusOK, gin.H{
				"success":             true,
				"message":             "Recording finish callback already handled",
				"task_status":         previousStatus,
				"upload_request_sent": false,
			})
			return
		case "failed", "cancelled":
			logger.Printf("%s finish callback rejected: current_status=%s", recorderTaskLogPrefix(deviceID, callback.TaskID), previousStatus)
			c.JSON(http.StatusConflict, gin.H{
				"error_msg": "task is not uploadable",
			})
			return
		case "pending", "ready", "in_progress":
		default:
			logger.Printf("%s finish callback rejected: current_status=%s", recorderTaskLogPrefix(deviceID, callback.TaskID), previousStatus)
			c.JSON(http.StatusConflict, gin.H{
				"error_msg": "task is not uploadable",
			})
			return
		}

		res, err := markOwnedTaskUploading(c.Request.Context(), h.db, deviceID, callback.TaskID)
		if err != nil {
			logger.Printf("%s failed to mark task uploading after finish callback: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to update task status"})
			return
		} else if n, _ := res.RowsAffected(); n > 0 {
			logger.Printf("%s task status updated: %s -> uploading reason=finish_callback", recorderTaskLogPrefix(deviceID, callback.TaskID), taskStatusLogValue(previousStatus, "unknown"))
		} else {
			currentStatus, _, statusErr := currentOwnedTaskStatus(c.Request.Context(), h.db, deviceID, callback.TaskID)
			if statusErr != nil {
				logger.Printf("%s failed to recheck task status after finish callback noop: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), statusErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task status"})
				return
			}
			if currentStatus == "uploading" || currentStatus == "completed" {
				logger.Printf("%s finish callback idempotent after noop: current_status=%s", recorderTaskLogPrefix(deviceID, callback.TaskID), currentStatus)
				c.JSON(http.StatusOK, gin.H{
					"success":             true,
					"message":             "Recording finish callback already handled",
					"task_status":         currentStatus,
					"upload_request_sent": false,
				})
				return
			}
			logger.Printf("%s task uploading transition skipped after finish callback", recorderTaskLogPrefix(deviceID, callback.TaskID))
			c.JSON(http.StatusConflict, gin.H{
				"error_msg": "task is not owned by device or is not uploadable",
			})
			return
		}
	}

	var dc *services.TransferConn
	if h.hub != nil {
		dc = h.hub.Get(deviceID)
	}
	if dc == nil {
		errorMessage := "transfer disconnected; upload_request not sent"
		if h.db != nil {
			if _, err := writeOwnedUploadingTaskError(c.Request.Context(), h.db, deviceID, callback.TaskID, errorMessage); err != nil {
				logger.Printf("%s failed to write upload_request error: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), err)
			}
		}
		logger.Printf("%s not found in hub, upload_request not sent", recorderTaskLogPrefix(deviceID, callback.TaskID))
		c.JSON(http.StatusOK, gin.H{
			"success":             true,
			"message":             "Recording finished; upload_request not sent because transfer is disconnected",
			"task_status":         "uploading",
			"upload_request_sent": false,
		})
		return
	}

	uploadRequest := map[string]interface{}{
		"type":     "upload_request",
		"task_id":  callback.TaskID,
		"priority": 1,
	}

	writeTimeout := h.axonTransferWriteTimeout()
	if err := h.hub.SendToDeviceWithTimeout(c.Request.Context(), deviceID, uploadRequest, writeTimeout); err != nil {
		if errors.Is(err, services.ErrTransferWriteTimeout) {
			logger.Printf("%s auto upload_request timed out after %s: %v", recorderTaskLogPrefix(deviceID, callback.TaskID), timeoutLogValue(writeTimeout), err)
		} else {
			logger.Printf("%s failed to send upload_request: %v", recorderTaskLogPrefix(deviceID, callback.TaskID), err)
		}
		errorMessage := "upload_request failed: " + err.Error()
		if h.db != nil {
			if _, writeErr := writeOwnedUploadingTaskError(c.Request.Context(), h.db, deviceID, callback.TaskID, errorMessage); writeErr != nil {
				logger.Printf("%s failed to write upload_request error: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), writeErr)
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"success":              true,
			"message":              "Recording finished; upload_request not sent",
			"task_status":          "uploading",
			"upload_request_sent":  false,
			"upload_request_error": err.Error(),
		})
		return
	}

	if h.db != nil {
		if _, err := clearOwnedUploadingTaskError(c.Request.Context(), h.db, deviceID, callback.TaskID); err != nil {
			logger.Printf("%s failed to clear upload_request error: err=%v", recorderTaskLogPrefix(deviceID, callback.TaskID), err)
		}
	}
	logger.Printf("%s successfully triggered upload", recorderTaskLogPrefix(deviceID, callback.TaskID))

	c.JSON(http.StatusOK, gin.H{
		"success":             true,
		"message":             "Upload triggered successfully",
		"task_status":         "uploading",
		"upload_request_sent": true,
	})
}

// GetTaskConfig returns the configuration for a task
//
// @Summary      Get task config
// @Description  Returns the configuration for a specific task by ID
// @Tags         tasks
// @Produce      json
// @Param        id  path      string  true  "Task ID"
// @Success      200 {object}  TaskConfig
// @Failure      404 {object}  map[string]string
// @Failure      409 {object}  map[string]string
// @Failure      500 {object}  map[string]string
// @Router       /tasks/{id}/config [get]
func (h *TaskHandler) GetTaskConfig(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "invalid task id"})
		return
	}
	if !h.authorizeCollectorTask(c, id) {
		return
	}

	var currentStatus string
	if err := h.db.Get(&currentStatus, `
		SELECT status
		FROM tasks
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error_msg": "Task not found: " + idStr})
			return
		}
		logger.Printf("[TASK] Failed to query task status for config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task"})
		return
	}
	if strings.TrimSpace(currentStatus) != "pending" {
		c.JSON(http.StatusConflict, gin.H{
			"code":           "task_not_configurable",
			"error_msg":      "Task is not configurable",
			"current_status": strings.TrimSpace(currentStatus),
		})
		return
	}

	type taskConfigRow struct {
		TaskID         string         `db:"task_id"`
		WorkstationID  sql.NullInt64  `db:"workstation_id"`
		RobotSerial    sql.NullString `db:"robot_serial"`
		CollectorName  sql.NullString `db:"collector_name"`
		Workstation    sql.NullString `db:"workstation_name"`
		OperatorName   sql.NullString `db:"operator_name"`
		Metadata       sql.NullString `db:"metadata"`
		WorkspaceID    sql.NullInt64  `db:"workspace_id"`
		DCPlanID       sql.NullInt64  `db:"dc_plan_id"`
		DCType         sql.NullString `db:"dc_type"`
		DCDeviceID     sql.NullInt64  `db:"dc_device_id"`
		TargetCount    sql.NullInt64  `db:"target_count"`
		TargetDuration sql.NullInt64  `db:"target_duration"`
	}

	var row taskConfigRow
	if err := h.db.Get(&row, `
		SELECT
			t.task_id AS task_id,
			t.workstation_id AS workstation_id,
			ws.robot_serial AS robot_serial,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(ws.name, '') AS workstation_name,
			dp.operator AS operator_name,
			t.metadata AS metadata,
			COALESCE(t.organization_id, ws.workspace_id) AS workspace_id,
			t.dc_plan_id AS dc_plan_id,
			dp.dc_type AS dc_type,
			dp.dc_device_id AS dc_device_id,
			dp.target_count AS target_count,
			dp.target_duration AS target_duration
		FROM tasks t
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN dc_plan dp ON dp.id = t.dc_plan_id AND dp.deleted_at IS NULL
		WHERE t.id = ? AND t.deleted_at IS NULL
		LIMIT 1
	`, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error_msg": "Task not found: " + idStr})
			return
		}
		logger.Printf("[TASK] Failed to query task for config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task"})
		return
	}

	if !row.WorkstationID.Valid {
		c.JSON(http.StatusConflict, gin.H{"error_msg": "Task has no workstation assigned"})
		return
	}
	if !row.RobotSerial.Valid || strings.TrimSpace(row.RobotSerial.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no robot_serial", row.WorkstationID.Int64)})
		return
	}
	if strings.TrimSpace(row.CollectorName.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no collector_name", row.WorkstationID.Int64)})
		return
	}
	if strings.TrimSpace(row.Workstation.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no name", row.WorkstationID.Int64)})
		return
	}
	executionConfig := taskExecutionConfigFromMetadata(row.Metadata.String)
	planSnapshot := taskPlanSnapshotFromMetadata(row.Metadata.String)

	taskConfig := TaskConfig{
		TaskID:            row.TaskID,
		DeviceID:          strings.TrimSpace(row.RobotSerial.String),
		DataCollectorID:   strings.TrimSpace(row.CollectorName.String),
		WorkstationID:     strings.TrimSpace(row.Workstation.String),
		OperatorName:      strings.TrimSpace(row.OperatorName.String),
		Topics:            executionConfig.Topics,
		StartCallbackURL:  h.callbackURLs.startURL(),
		FinishCallbackURL: h.callbackURLs.finishURL(),
		UserToken:         "",
	}
	if row.WorkspaceID.Valid {
		taskConfig.WorkspaceID = &row.WorkspaceID.Int64
	}
	if row.DCPlanID.Valid {
		taskConfig.DCPlanID = &row.DCPlanID.Int64
	}
	if planSnapshot.DCPlanName != nil {
		taskConfig.DCPlanName = strings.TrimSpace(*planSnapshot.DCPlanName)
	}
	if planSnapshot.DCProjectDescription != nil {
		taskConfig.DCProjectDescription = strings.TrimSpace(*planSnapshot.DCProjectDescription)
	}
	if planSnapshot.DCTaskDescription != nil {
		taskConfig.DCTaskDescription = strings.TrimSpace(*planSnapshot.DCTaskDescription)
	}
	if row.DCType.Valid {
		taskConfig.DCType = strings.TrimSpace(row.DCType.String)
	}
	if row.DCDeviceID.Valid {
		taskConfig.DCDeviceID = &row.DCDeviceID.Int64
	}
	if row.TargetCount.Valid {
		taskConfig.PlanTargetCount = &row.TargetCount.Int64
	}
	if row.TargetDuration.Valid {
		taskConfig.PlanTargetDuration = &row.TargetDuration.Int64
	}
	if planSnapshot.Operator != nil {
		taskConfig.OperatorName = strings.TrimSpace(*planSnapshot.Operator)
	}
	if planSnapshot.DCType != nil {
		taskConfig.DCType = strings.TrimSpace(*planSnapshot.DCType)
	}
	if planSnapshot.DCDeviceID != nil {
		taskConfig.DCDeviceID = planSnapshot.DCDeviceID
	}
	if planSnapshot.TargetCount != nil {
		taskConfig.PlanTargetCount = planSnapshot.TargetCount
	}
	if planSnapshot.TargetDuration != nil {
		taskConfig.PlanTargetDuration = planSnapshot.TargetDuration
	}

	c.JSON(http.StatusOK, taskConfig)
}

func (h *TaskHandler) authorizeCollectorTask(c *gin.Context, taskID int64) bool {
	claims := middleware.GetClaims(c)
	if claims == nil || claims.Role != "data_collector" {
		return true
	}
	var taskScope struct {
		CollectorID int64 `db:"data_collector_id"`
		WorkspaceID int64 `db:"workspace_id"`
	}
	if err := h.db.GetContext(c.Request.Context(), &taskScope, `
		SELECT ws.data_collector_id, COALESCE(t.organization_id, ws.workspace_id) AS workspace_id
		FROM tasks t
		INNER JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		WHERE t.id = ? AND t.deleted_at IS NULL
		LIMIT 1
	`, taskID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error_msg": "task not found"})
			return false
		}
		logger.Printf("[TASK] Failed to authorize collector task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to authorize task"})
		return false
	}
	if taskScope.CollectorID != claims.CollectorID {
		c.JSON(http.StatusForbidden, gin.H{"error_msg": "task access denied"})
		return false
	}
	allowed, err := services.OperatorHasWorkspaceAccess(c.Request.Context(), h.db, claims.OperatorID, taskScope.WorkspaceID)
	if err != nil {
		logger.Printf("[TASK] Failed to authorize task Workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to authorize task"})
		return false
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error_msg": "workspace access denied"})
		return false
	}
	return true
}

type taskExecutionConfig struct {
	Topics []string `json:"topics"`
}

type taskPlanSnapshot struct {
	DCPlanName           *string `json:"dc_plan_name"`
	DCProjectDescription *string `json:"dc_project_description"`
	DCTaskDescription    *string `json:"dc_task_description"`
	Operator             *string `json:"operator"`
	DCType               *string `json:"dc_type"`
	DCDeviceID           *int64  `json:"dc_device_id"`
	TargetCount          *int64  `json:"target_count"`
	TargetDuration       *int64  `json:"target_duration"`
}

func taskExecutionConfigFromMetadata(raw string) taskExecutionConfig {
	config := taskExecutionConfig{Topics: []string{}}
	if strings.TrimSpace(raw) == "" {
		return config
	}
	var metadata struct {
		ExecutionConfig taskExecutionConfig `json:"execution_config"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return config
	}
	if metadata.ExecutionConfig.Topics != nil {
		config.Topics = metadata.ExecutionConfig.Topics
	}
	return config
}

func taskPlanSnapshotFromMetadata(raw string) taskPlanSnapshot {
	snapshot := taskPlanSnapshot{}
	if strings.TrimSpace(raw) == "" {
		return snapshot
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return taskPlanSnapshot{}
	}
	return snapshot
}

func applyTaskPlanSnapshot(item *TaskListItem, raw string) {
	if item == nil {
		return
	}
	snapshot := taskPlanSnapshotFromMetadata(raw)
	if snapshot.DCPlanName != nil {
		value := strings.TrimSpace(*snapshot.DCPlanName)
		item.DCPlanName = &value
	}
	if snapshot.DCProjectDescription != nil {
		item.DCProjectDescription = strings.TrimSpace(*snapshot.DCProjectDescription)
	}
	if snapshot.DCTaskDescription != nil {
		item.DCTaskDescription = strings.TrimSpace(*snapshot.DCTaskDescription)
	}
}
