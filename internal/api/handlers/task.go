// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
)

func newPublicTaskID(now time.Time, seq int) (string, error) {
	// Format: task_YYYYMMDD_HHMMSS_mmm_<seq>_<rand8>
	// - millisecond timestamp makes it readable
	// - seq differentiates multiple creates in same millisecond
	// - rand suffix prevents collision across processes/hosts
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"task_%s_%03d_%02d_%s",
		now.UTC().Format("20060102_150405"),
		now.UTC().Nanosecond()/1_000_000,
		seq%100,
		hex.EncodeToString(b),
	), nil
}

// TaskHandler handles task-related HTTP requests
type TaskHandler struct {
	db  *sqlx.DB
	hub *services.TransferHub
}

// NewTaskHandler creates a new TaskHandler
func NewTaskHandler(db *sqlx.DB, hub *services.TransferHub) *TaskHandler {
	return &TaskHandler{
		db:  db,
		hub: hub,
	}
}

// TaskConfig represents the task configuration response
type TaskConfig struct {
	TaskID             string   `json:"task_id"`
	DeviceID           string   `json:"device_id"`
	DataCollectorID    string   `json:"data_collector_id"`
	OrderID            string   `json:"order_id"`
	Factory            string   `json:"factory"`
	Scene              string   `json:"scene"`
	WorkstationID      string   `json:"workstation_id"`
	Subscene           string   `json:"subscene"`
	SubsceneID         string   `json:"subscene_id"`
	InitialSceneLayout string   `json:"initial_scene_layout"`
	Skills             []string `json:"skills"`
	SOPID              string   `json:"sop_id"`
	Topics             []string `json:"topics"`
	StartCallbackURL   string   `json:"start_callback_url"`
	FinishCallbackURL  string   `json:"finish_callback_url"`
	UserToken          string   `json:"user_token"`
}

// RegisterRoutes registers task-related routes
func (h *TaskHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/tasks", h.CreateTask)
	apiV1.GET("/tasks", h.ListTasks)
	apiV1.GET("/tasks/:id", h.GetTask)
	apiV1.PUT("/tasks/:id", h.UpdateTask)
	apiV1.GET("/tasks/:id/config", h.GetTaskConfig)
}

var validTaskStatuses = map[string]struct{}{
	"pending":     {},
	"ready":       {},
	"in_progress": {},
	"completed":   {},
	"failed":      {},
	"cancelled":   {},
}

// TaskListItem represents a task item in list responses.
type TaskListItem struct {
	ID            string  `json:"id" db:"id"`
	TaskID        string  `json:"task_id" db:"task_id"`
	BatchID       string  `json:"batch_id" db:"batch_id"`
	OrderID       string  `json:"order_id" db:"order_id"`
	SOPID         string  `json:"sop_id" db:"sop_id"`
	WorkstationID *string `json:"workstation_id" db:"workstation_id"`
	SceneID       string  `json:"scene_id" db:"scene_id"`
	SceneName     string  `json:"scene_name" db:"scene_name"`
	SubsceneID    string  `json:"subscene_id" db:"subscene_id"`
	SubsceneName  string  `json:"subscene_name" db:"subscene_name"`
	Status        string  `json:"status" db:"status"`
	AssignedAt    *string `json:"assigned_at" db:"assigned_at"`
}

// ListTasksResponse represents the response body for listing tasks.
type ListTasksResponse struct {
	Tasks  []TaskListItem `json:"tasks"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// TaskEpisodeDetail represents the episode information attached to a task.
type TaskEpisodeDetail struct {
	ID string `json:"id" db:"id"`
}

// TaskDetailResponse represents the response body for getting a task by ID.
type TaskDetailResponse struct {
	ID             string             `json:"id" db:"id"`
	TaskID         string             `json:"task_id" db:"task_id"`
	BatchID        string             `json:"batch_id" db:"batch_id"`
	BatchName      string             `json:"batch_name" db:"batch_name"`
	OrderID        string             `json:"order_id" db:"order_id"`
	SOPID          string             `json:"sop_id" db:"sop_id"`
	WorkstationID  *string            `json:"workstation_id" db:"workstation_id"`
	SceneID        string             `json:"scene_id" db:"scene_id"`
	SceneName      string             `json:"scene_name" db:"scene_name"`
	SubsceneID     string             `json:"subscene_id" db:"subscene_id"`
	SubsceneName   string             `json:"subscene_name" db:"subscene_name"`
	FactoryID      *string            `json:"factory_id" db:"factory_id"`
	OrganizationID *int64             `json:"organization_id" db:"organization_id"`
	Status         string             `json:"status" db:"status"`
	CreatedAt      *string            `json:"created_at" db:"created_at"`
	AssignedAt     *string            `json:"assigned_at" db:"assigned_at"`
	StartedAt      *string            `json:"started_at" db:"started_at"`
	CompletedAt    *string            `json:"completed_at" db:"completed_at"`
	Episode        *TaskEpisodeDetail `json:"episode"`
	EpisodeID      *string            `json:"-" db:"episode_id"`
}

// UpdateTaskRequest represents the request body for updating a task status.
type UpdateTaskRequest struct {
	Status    string `json:"status"`
	UpdatedBy string `json:"updated_by"`
}

// UpdateTaskResponse represents the response body for updating a task status.
type UpdateTaskResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

var validTaskStatusTransitions = map[string]map[string]struct{}{
	"pending": {
		"ready":     {},
		"cancelled": {},
	},
	"ready": {
		"pending": {},
	},
}

// ListTasks handles task listing requests with optional filtering.
//
// @Summary      List tasks
// @Description  Lists tasks with optional workstation and status filters
// @Tags         tasks
// @Produce      json
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

	workstationID := strings.TrimSpace(c.Query("workstation_id"))
	status := strings.TrimSpace(c.Query("status"))

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

	conditions := []string{"deleted_at IS NULL"}
	args := make([]interface{}, 0, 4)

	if workstationID != "" {
		conditions = append(conditions, "CAST(workstation_id AS CHAR) = ?")
		args = append(args, workstationID)
	}

	if status != "" {
		conditions = append(conditions, "status = ?")
		args = append(args, status)
	}

	whereClause := strings.Join(conditions, " AND ")

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM tasks WHERE %s", whereClause)
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[TASK] Failed to count tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}

	queryArgs := append(append([]interface{}{}, args...), limit, offset)
	listQuery := fmt.Sprintf(`SELECT
		CAST(id AS CHAR) AS id,
		task_id AS task_id,
		CAST(batch_id AS CHAR) AS batch_id,
		CAST(order_id AS CHAR) AS order_id,
		CAST(sop_id AS CHAR) AS sop_id,
		CASE WHEN workstation_id IS NULL THEN NULL ELSE CAST(workstation_id AS CHAR) END AS workstation_id,
		CAST(scene_id AS CHAR) AS scene_id,
		COALESCE(scene_name, '') AS scene_name,
		CAST(subscene_id AS CHAR) AS subscene_id,
		COALESCE(subscene_name, '') AS subscene_name,
		status,
		CASE WHEN assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(assigned_at, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') END AS assigned_at
		FROM tasks
		WHERE %s
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, whereClause)

	items := make([]TaskListItem, 0)
	if err := h.db.Select(&items, listQuery, queryArgs...); err != nil {
		logger.Printf("[TASK] Failed to query tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}

	c.JSON(http.StatusOK, ListTasksResponse{
		Tasks:  items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
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

	var task TaskDetailResponse
	query := `SELECT
		CAST(t.id AS CHAR) AS id,
		t.task_id AS task_id,
		CAST(t.batch_id AS CHAR) AS batch_id,
		COALESCE(t.batch_name, '') AS batch_name,
		CAST(t.order_id AS CHAR) AS order_id,
		CAST(t.sop_id AS CHAR) AS sop_id,
		CASE WHEN t.workstation_id IS NULL THEN NULL ELSE CAST(t.workstation_id AS CHAR) END AS workstation_id,
		CAST(t.scene_id AS CHAR) AS scene_id,
		COALESCE(t.scene_name, '') AS scene_name,
		CAST(t.subscene_id AS CHAR) AS subscene_id,
		COALESCE(t.subscene_name, '') AS subscene_name,
		CASE WHEN t.factory_id IS NULL THEN NULL ELSE CAST(t.factory_id AS CHAR) END AS factory_id,
		t.organization_id AS organization_id,
		t.status,
		CASE WHEN t.created_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.created_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS created_at,
		CASE WHEN t.assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.assigned_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS assigned_at,
		CASE WHEN t.started_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.started_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS started_at,
		CASE WHEN t.completed_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.completed_at, @@session.time_zone, '+00:00'), '%Y-%m-%dT%H:%i:%sZ') END AS completed_at,
		e.episode_id AS episode_id
		FROM tasks t
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

	if task.EpisodeID != nil {
		task.Episode = &TaskEpisodeDetail{ID: *task.EpisodeID}
	}

	c.JSON(http.StatusOK, task)
}

// UpdateTask handles task status update requests.
//
// @Summary      Update task
// @Description  Updates task status with restricted state transitions
// @Tags         tasks
// @Accept       json
// @Produce      json
// @Param        id    path      string             true  "Task ID"
// @Param        body  body      UpdateTaskRequest  true  "Task update payload"
// @Success      200   {object}  UpdateTaskResponse
// @Failure      400   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /tasks/{id} [put]
func (h *TaskHandler) UpdateTask(c *gin.Context) {
	idStr := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "invalid task id"})
		return
	}
	if idStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "task id is required"})
		return
	}

	var req UpdateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	req.Status = strings.TrimSpace(req.Status)
	req.UpdatedBy = strings.TrimSpace(req.UpdatedBy)

	if req.Status == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "status is required"})
		return
	}

	if _, ok := validTaskStatuses[req.Status]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "invalid status"})
		return
	}

	if req.UpdatedBy == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "updated_by is required"})
		return
	}

	var taskRow struct {
		Status string `db:"status"`
	}
	err = h.db.Get(&taskRow, "SELECT status FROM tasks WHERE id = ? AND deleted_at IS NULL", id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error_msg": "Task not found: " + idStr})
		return
	}
	if err != nil {
		logger.Printf("[TASK] Failed to query task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query task"})
		return
	}

	if _, ok := validTaskStatusTransitions[taskRow.Status][req.Status]; !ok {
		c.JSON(http.StatusConflict, gin.H{
			"error_msg":        fmt.Sprintf("Cannot transition from '%s' to '%s'", taskRow.Status, req.Status),
			"current_status":   taskRow.Status,
			"requested_status": req.Status,
		})
		return
	}

	now := time.Now().UTC()
	result, err := h.db.Exec(
		"UPDATE tasks SET status = ?, updated_at = ?, ready_at = CASE WHEN ? = 'ready' THEN ? ELSE ready_at END WHERE id = ? AND status = ? AND deleted_at IS NULL",
		req.Status,
		now,
		req.Status,
		now,
		id,
		taskRow.Status,
	)
	if err != nil {
		logger.Printf("[TASK] Failed to update task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to update task"})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[TASK] Failed to get rows affected: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to verify update"})
		return
	}

	if rowsAffected == 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error_msg":        fmt.Sprintf("Cannot transition from '%s' to '%s'", taskRow.Status, req.Status),
			"current_status":   taskRow.Status,
			"requested_status": req.Status,
		})
		return
	}

	c.JSON(http.StatusOK, UpdateTaskResponse{
		ID:        idStr,
		Status:    req.Status,
		UpdatedAt: now.Format(time.RFC3339),
	})
}

// CreateTaskResponse represents the response body for creating a task.
type CreateTaskResponse struct {
	ID        string               `json:"id"`
	TaskID    string               `json:"task_id"`
	Status    string               `json:"status"`
	CreatedAt string               `json:"created_at"`
	Tasks     []CreateTaskResponse `json:"tasks"`
}

// CreateTaskRequest represents the request body for creating a task.
type CreateTaskRequest struct {
	OrderID       int64 `json:"order_id" binding:"required"`
	SOPID         int64 `json:"sop_id" binding:"required"`
	SubsceneID    int64 `json:"subscene_id" binding:"required"`
	WorkstationID int64 `json:"workstation_id" binding:"required"`
	Quantity      *int  `json:"quantity,omitempty"`
}

// CreateTask handles task creation requests.
//
// @Summary      Create task
// @Description  Creates a new task with pending status
// @Tags         tasks
// @Accept       json
// @Produce      json
// @Success      201  {object}  CreateTaskResponse
// @Failure      500  {object}  map[string]string
// @Router       /tasks [post]
func (h *TaskHandler) CreateTask(c *gin.Context) {
	var req CreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	now := time.Now().UTC()
	quantity := 1
	if req.Quantity != nil {
		quantity = *req.Quantity
	}
	if quantity < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "Invalid quantity: must be >= 1"})
		return
	}
	if quantity > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error_msg": "Invalid quantity: must be <= 1000"})
		return
	}

	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[TASK] Failed to start transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Validate referenced rows exist (all IDs are table `id` fields).
	var targetCount int
	// Lock the order row to coordinate concurrent task generation for this order.
	if err := tx.Get(&targetCount, "SELECT target_count FROM orders WHERE id = ? AND deleted_at IS NULL LIMIT 1 FOR UPDATE", req.OrderID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error_msg": fmt.Sprintf("Invalid order_id: %d", req.OrderID)})
			return
		}
		logger.Printf("[TASK] Failed to validate order_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	// Lock existing tasks for this order, so concurrent inserts will serialize under REPEATABLE READ.
	var lockedTaskIDs []int64
	if err := tx.Select(&lockedTaskIDs, "SELECT id FROM tasks WHERE order_id = ? AND deleted_at IS NULL FOR UPDATE", req.OrderID); err != nil {
		logger.Printf("[TASK] Failed to lock existing tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	existingCount := len(lockedTaskIDs)
	if existingCount+quantity > targetCount {
		remaining := targetCount - existingCount
		if remaining < 0 {
			remaining = 0
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": fmt.Sprintf("Invalid quantity: order target_count=%d, existing_tasks=%d, remaining=%d", targetCount, existingCount, remaining),
		})
		return
	}

	var exists int
	if err := tx.Get(&exists, "SELECT 1 FROM sops WHERE id = ? AND deleted_at IS NULL LIMIT 1", req.SOPID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error_msg": fmt.Sprintf("Invalid sop_id: %d", req.SOPID)})
			return
		}
		logger.Printf("[TASK] Failed to validate sop_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	type subsceneRow struct {
		ID      int64  `db:"id"`
		SceneID int64  `db:"scene_id"`
		Scene   string `db:"scene_name"`
		Name    string `db:"name"`
		Layout  string `db:"initial_scene_layout"`
	}
	var subscene subsceneRow
	if err := tx.Get(&subscene, `
		SELECT
			ss.id,
			ss.scene_id,
			s.name AS scene_name,
			ss.name,
			COALESCE(ss.initial_scene_layout, '') AS initial_scene_layout
		FROM subscenes ss
		JOIN scenes s ON s.id = ss.scene_id AND s.deleted_at IS NULL
		WHERE ss.id = ? AND ss.deleted_at IS NULL
		LIMIT 1`, req.SubsceneID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error_msg": fmt.Sprintf("Invalid subscene_id: %d", req.SubsceneID)})
			return
		}
		logger.Printf("[TASK] Failed to validate subscene_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	type workstationRow struct {
		ID        int64 `db:"id"`
		FactoryID int64 `db:"factory_id"`
	}
	var ws workstationRow
	if err := tx.Get(&ws, "SELECT id, factory_id FROM workstations WHERE id = ? AND deleted_at IS NULL LIMIT 1", req.WorkstationID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error_msg": fmt.Sprintf("Invalid workstation_id: %d", req.WorkstationID)})
			return
		}
		logger.Printf("[TASK] Failed to validate workstation_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}

	// Ensure a batch exists for (order_id, workstation_id). Prefer active/pending, otherwise create.
	type batchRow struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	var batch batchRow
	batchQuery := `
		SELECT id, name
		FROM batches
		WHERE order_id = ? AND workstation_id = ? AND deleted_at IS NULL AND status IN ('pending', 'active')
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	err = tx.Get(&batch, batchQuery, req.OrderID, req.WorkstationID)
	if err != nil && err != sql.ErrNoRows {
		logger.Printf("[TASK] Failed to query batch: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}
	if err == sql.ErrNoRows {
		batchIDStr := now.Format("batch_20060102_150405")
		batchName := fmt.Sprintf("Batch %s (order=%d ws=%d)", batchIDStr, req.OrderID, req.WorkstationID)
		res, err := tx.Exec(
			`INSERT INTO batches (
				batch_id,
				order_id,
				workstation_id,
				name,
				status,
				created_at,
				updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			batchIDStr,
			req.OrderID,
			req.WorkstationID,
			batchName,
			"pending",
			now,
			now,
		)
		if err != nil {
			logger.Printf("[TASK] Failed to insert batch: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
			return
		}
		newID, err := res.LastInsertId()
		if err != nil {
			logger.Printf("[TASK] Failed to get batch insert id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
			return
		}
		batch.ID = newID
		batch.Name = batchName
	}

	// Denormalized filtering fields
	var organizationID int64
	if err := tx.Get(&organizationID, "SELECT organization_id FROM factories WHERE id = ? AND deleted_at IS NULL LIMIT 1", ws.FactoryID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error_msg": "workstation factory not found"})
			return
		}
		logger.Printf("[TASK] Failed to resolve organization_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}

	created := make([]CreateTaskResponse, 0, quantity)
	for i := 0; i < quantity; i++ {
		taskID, err := newPublicTaskID(now, i)
		if err != nil {
			logger.Printf("[TASK] Failed to generate task_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
			return
		}
		resTask, err := tx.Exec(
			`INSERT INTO tasks (
				task_id,
				batch_id,
				order_id,
				sop_id,
				workstation_id,
				scene_id,
				subscene_id,
				batch_name,
				scene_name,
				subscene_name,
				factory_id,
				organization_id,
				initial_scene_layout,
				status,
				created_at,
				updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID,
			batch.ID,
			req.OrderID,
			req.SOPID,
			req.WorkstationID,
			subscene.SceneID,
			req.SubsceneID,
			batch.Name,
			subscene.Scene,
			subscene.Name,
			ws.FactoryID,
			organizationID,
			subscene.Layout,
			"pending",
			now,
			now,
		)
		if err != nil {
			logger.Printf("[TASK] Failed to insert task: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
			return
		}
		newTaskID, err := resTask.LastInsertId()
		if err != nil {
			logger.Printf("[TASK] Failed to get task insert id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
			return
		}
		created = append(created, CreateTaskResponse{
			ID:        fmt.Sprintf("%d", newTaskID),
			TaskID:    taskID,
			Status:    "pending",
			CreatedAt: now.Format(time.RFC3339),
		})
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[TASK] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "failed to create task"})
		return
	}

	resp := CreateTaskResponse{
		Status:    "pending",
		CreatedAt: now.Format(time.RFC3339),
		Tasks:     created,
	}
	// Backwards compatibility for clients expecting a single task.
	if len(created) > 0 {
		resp.ID = created[0].ID
		resp.TaskID = created[0].TaskID
	}
	c.JSON(http.StatusCreated, resp)
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
		Scene    string   `json:"scene"`
		Subscene string   `json:"subscene"`
		Skills   []string `json:"skills"`
		Factory  string   `json:"factory"`
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

	logger.Printf("[RECORDER] Device %s: received start callback for task=%s", callback.DeviceID, callback.TaskID)

	// Validate required fields
	if callback.TaskID == "" {
		logger.Printf("[RECORDER] Device %s: Missing task_id in callback", callback.DeviceID)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: task_id",
		})
		return
	}

	now := time.Now()
	nowStr := now.Format(time.RFC3339)
	c.JSON(http.StatusOK, gin.H{
		"status":          "acknowledged",
		"task_status":     "unknown",
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
		logger.Printf("[RECORDER] Failed to parse callback: missing output_path for task_id=%s", callback.TaskID)
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

	logger.Printf("[RECORDER] Device %s: received finish callback for task=%s", callback.DeviceID, callback.TaskID)

	dc := h.hub.Get(deviceID)
	if dc == nil {
		// TODO: add status pending_upload, when device reconnects, check for any pending_upload tasks and trigger upload then
		logger.Printf("[RECORDER] Device %s: Not found in hub for task=%s, cannot trigger upload", deviceID, callback.TaskID)
		c.JSON(http.StatusConflict, gin.H{
			"error_msg": "Recording finished, device not connected for auto-upload",
		})
		return
	}

	uploadRequest := map[string]interface{}{
		"type":     "upload_request",
		"task_id":  callback.TaskID,
		"priority": 1,
	}

	if err := h.hub.SendToDevice(c.Request.Context(), deviceID, uploadRequest); err != nil {
		logger.Printf("[RECORDER] Failed to send upload_request to device %s: %v", deviceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error_msg": "Failed to trigger upload: " + err.Error(),
		})
		return
	}

	logger.Printf("[RECORDER] Device %s: successfully triggered upload for task_id=%s", deviceID, callback.TaskID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Upload triggered successfully",
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

	type taskConfigRow struct {
		TaskID        string         `db:"task_id"`
		WorkstationID sql.NullInt64  `db:"workstation_id"`
		RobotSerial   sql.NullString `db:"robot_serial"`
		RobotID       sql.NullInt64  `db:"robot_id"`
		CollectorName sql.NullString `db:"collector_name"`
		OrderName     sql.NullString `db:"order_name"`
		Workstation   sql.NullString `db:"workstation_name"`
		FactoryName   sql.NullString `db:"factory_name"`
		SceneName     sql.NullString `db:"scene_name"`
		SubsceneName  sql.NullString `db:"subscene_name"`
		Layout        sql.NullString `db:"initial_scene_layout"`
		SOPName       sql.NullString `db:"sop_name"`
		SkillSequence sql.NullString `db:"skill_sequence"`
		ROSTopics     sql.NullString `db:"ros_topics"`
	}

	var row taskConfigRow
	if err := h.db.Get(&row, `
		SELECT
			t.task_id AS task_id,
			t.workstation_id AS workstation_id,
			ws.robot_serial AS robot_serial,
			ws.robot_id AS robot_id,
			COALESCE(ws.collector_name, '') AS collector_name,
			COALESCE(o.name, '') AS order_name,
			COALESCE(ws.name, '') AS workstation_name,
			COALESCE(f.name, '') AS factory_name,
			COALESCE(t.scene_name, '') AS scene_name,
			COALESCE(t.subscene_name, '') AS subscene_name,
			COALESCE(t.initial_scene_layout, '') AS initial_scene_layout,
			s.name AS sop_name,
			COALESCE(s.skill_sequence, '[]') AS skill_sequence,
			COALESCE(rt.ros_topics, '[]') AS ros_topics
		FROM tasks t
		LEFT JOIN factories f ON f.id = t.factory_id AND f.deleted_at IS NULL
		LEFT JOIN orders o ON o.id = t.order_id AND o.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = t.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		LEFT JOIN sops s ON s.id = t.sop_id AND s.deleted_at IS NULL
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
	if !row.RobotID.Valid || row.RobotID.Int64 <= 0 {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no robot_id", row.WorkstationID.Int64)})
		return
	}
	if strings.TrimSpace(row.CollectorName.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no collector_name", row.WorkstationID.Int64)})
		return
	}
	if strings.TrimSpace(row.OrderName.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": "Task order_id not found"})
		return
	}
	if strings.TrimSpace(row.Workstation.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Workstation %d has no name", row.WorkstationID.Int64)})
		return
	}
	if strings.TrimSpace(row.FactoryName.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": "Task factory_id not found"})
		return
	}
	if !row.ROSTopics.Valid || strings.TrimSpace(row.ROSTopics.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": fmt.Sprintf("Robot %d has no robot_type ros_topics", row.RobotID.Int64)})
		return
	}
	if !row.SOPName.Valid || strings.TrimSpace(row.SOPName.String) == "" {
		c.JSON(http.StatusConflict, gin.H{"error_msg": "Task sop_id not found"})
		return
	}

	// Resolve skill ids (from sop.skill_sequence) to skill names, preserving order.
	skillIDs := parseJSONArray(row.SkillSequence.String)
	skills := make([]string, 0, len(skillIDs))
	if len(skillIDs) > 0 {
		type skillRow struct {
			ID   string `db:"id"`
			Name string `db:"name"`
		}
		query, args, err := sqlx.In(
			`SELECT CAST(id AS CHAR) AS id, name FROM skills WHERE deleted_at IS NULL AND id IN (?)`,
			skillIDs,
		)
		if err != nil {
			logger.Printf("[TASK] Failed to build skill query: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query skills"})
			return
		}
		query = h.db.Rebind(query)
		var rows []skillRow
		if err := h.db.Select(&rows, query, args...); err != nil {
			logger.Printf("[TASK] Failed to query skills: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to query skills"})
			return
		}
		nameByID := make(map[string]string, len(rows))
		for _, r := range rows {
			nameByID[strings.TrimSpace(r.ID)] = strings.TrimSpace(r.Name)
		}
		for _, id := range skillIDs {
			if name, ok := nameByID[strings.TrimSpace(id)]; ok && name != "" {
				skills = append(skills, name)
			}
		}
	}

	taskConfig := TaskConfig{
		TaskID:             row.TaskID,
		DeviceID:           strings.TrimSpace(row.RobotSerial.String),
		DataCollectorID:    strings.TrimSpace(row.CollectorName.String),
		OrderID:            strings.TrimSpace(row.OrderName.String),
		Factory:            strings.TrimSpace(row.FactoryName.String),
		Scene:              strings.TrimSpace(row.SceneName.String),
		WorkstationID:      strings.TrimSpace(row.Workstation.String),
		Subscene:           strings.TrimSpace(row.SubsceneName.String),
		SubsceneID:         strings.TrimSpace(row.SubsceneName.String),
		InitialSceneLayout: strings.TrimSpace(row.Layout.String),
		Skills:             skills,
		SOPID:              strings.TrimSpace(row.SOPName.String),
		Topics:             parseJSONArray(row.ROSTopics.String),
		StartCallbackURL:   "http://keystone.factory.internal/api/v1/callbacks/start",
		FinishCallbackURL:  "http://keystone.factory.internal/api/v1/callbacks/finish",
		UserToken:          "",
	}

	c.JSON(http.StatusOK, taskConfig)
}
