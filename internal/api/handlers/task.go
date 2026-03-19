// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/services"
)

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
	TaskID             string          `json:"task_id"`
	DeviceID           string          `json:"device_id"`
	Scene              string          `json:"scene"`
	Subscene           string          `json:"subscene"`
	InitialSceneLayout string          `json:"initial_scene_layout"`
	Skills             []string        `json:"skills"`
	SOPID              string          `json:"sop_id"`
	Topics             []string        `json:"topics"`
	StartCallbackURL   string          `json:"start_callback_url"`
	FinishCallbackURL  string          `json:"finish_callback_url"`
	UserToken          string          `json:"user_token"`
	RecordingConfig    RecordingConfig `json:"recording_config"`
}

// RecordingConfig represents recording configuration settings
type RecordingConfig struct {
	MaxDurationSec int    `json:"max_duration_sec"`
	Compression    string `json:"compression"`
}

// RegisterRoutes registers task-related routes
func (h *TaskHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/tasks", h.CreateTask)
	apiV1.GET("/tasks", h.ListTasks)
	apiV1.GET("/tasks/:id", h.GetTask)
	apiV1.PATCH("/tasks/:id", h.UpdateTask)
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
		log.Printf("[ListTasks] Failed to count tasks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list tasks"})
		return
	}

	queryArgs := append(append([]interface{}{}, args...), limit, offset)
	listQuery := fmt.Sprintf(`SELECT
		task_id AS id,
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
		log.Printf("[ListTasks] Failed to query tasks: %v", err)
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
	taskID := strings.TrimSpace(c.Param("id"))

	var task TaskDetailResponse
	query := `SELECT
		t.task_id AS id,
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
		CASE WHEN t.assigned_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.assigned_at, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') END AS assigned_at,
		CASE WHEN t.started_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.started_at, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') END AS started_at,
		CASE WHEN t.completed_at IS NULL THEN NULL ELSE DATE_FORMAT(CONVERT_TZ(t.completed_at, @@session.time_zone, '+00:00'), '%%Y-%%m-%%dT%%H:%%i:%%sZ') END AS completed_at,
		e.episode_id AS episode_id
		FROM tasks t
		LEFT JOIN episodes e ON e.task_id = t.id AND e.deleted_at IS NULL
		WHERE t.task_id = ? AND t.deleted_at IS NULL
		LIMIT 1`

	err := h.db.Get(&task, query, taskID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{
			"error_msg": "Task not found: " + taskID,
		})
		return
	}

	if err != nil {
		log.Printf("[GetTask] Failed to query task: %v", err)
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
// @Router       /tasks/{id} [patch]
func (h *TaskHandler) UpdateTask(c *gin.Context) {
	taskID := strings.TrimSpace(c.Param("id"))
	if taskID == "" {
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
	err := h.db.Get(&taskRow, "SELECT status FROM tasks WHERE task_id = ? AND deleted_at IS NULL", taskID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error_msg": "Task not found: " + taskID})
		return
	}
	if err != nil {
		log.Printf("[UpdateTask] Failed to query task: %v", err)
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
		"UPDATE tasks SET status = ?, updated_at = ?, ready_at = CASE WHEN ? = 'ready' THEN ? ELSE ready_at END WHERE task_id = ? AND status = ? AND deleted_at IS NULL",
		req.Status,
		now,
		req.Status,
		now,
		taskID,
		taskRow.Status,
	)
	if err != nil {
		log.Printf("[UpdateTask] Failed to update task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error_msg": "Failed to update task"})
		return
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("[UpdateTask] Failed to get rows affected: %v", err)
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
		ID:        taskID,
		Status:    req.Status,
		UpdatedAt: now.Format(time.RFC3339),
	})
}

// CreateTaskResponse represents the response body for creating a task.
type CreateTaskResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
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
	now := time.Now().UTC()
	taskID := now.Format("task_20060102_150405")

	_, err := h.db.Exec(
		`INSERT INTO tasks (
			task_id,
			batch_id,
			order_id,
			sop_id,
			workstation_id,
			scene_id,
			subscene_id,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID,
		0,
		0,
		0,
		nil,
		0,
		0,
		"pending",
		now,
		now,
	)
	if err != nil {
		log.Printf("[CreateTask] Failed to insert task: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "failed to create task",
		})
		return
	}

	c.JSON(http.StatusCreated, CreateTaskResponse{
		ID:        taskID,
		Status:    "pending",
		CreatedAt: now.Format(time.RFC3339),
	})
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
		log.Printf("[RECORDER] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	log.Printf("[RECORDER] Device %s: received start callback for task=%s", callback.DeviceID, callback.TaskID)

	// Validate required fields
	if callback.TaskID == "" {
		log.Printf("[RECORDER] Device %s: Missing task_id in callback", callback.DeviceID)
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
		log.Printf("[RECORDER] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	// Validate required fields
	if callback.TaskID == "" {
		log.Printf("[RECORDER] Missing task_id in callback")
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: task_id",
		})
		return
	}

	if callback.OutputPath == "" {
		log.Printf("[RECORDER] No output_path provided for task_id=%s, skipping upload", callback.TaskID)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: output_path",
		})
		return
	}

	deviceID := callback.DeviceID
	if deviceID == "" {
		log.Printf("[RECORDER] Missing device_id in callback")
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: device_id",
		})
		return
	}

	log.Printf("[RECORDER] Device %s: received finish callback for task=%s", callback.DeviceID, callback.TaskID)

	dc := h.hub.Get(deviceID)
	if dc == nil {
		// TODO: add status pending_upload, when device reconnects, check for any pending_upload tasks and trigger upload then
		log.Printf("[RECORDER] Device %s: Not found in hub for task=%s, cannot trigger upload", deviceID, callback.TaskID)
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
		log.Printf("[RECORDER] Failed to send upload_request to device %s: %v", deviceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error_msg": "Failed to trigger upload: " + err.Error(),
		})
		return
	}

	log.Printf("[RECORDER] Device %s: successfully triggered upload for task_id=%s", deviceID, callback.TaskID)

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
	taskID := c.Param("id")

	// Return mocked data
	// #nosec G101 - This is a mock response for testing purposes.
	taskConfig := TaskConfig{
		TaskID:             taskID,
		DeviceID:           "robot_arm_001",
		Scene:              "commercial_kitchen",
		Subscene:           "dishwashing_station",
		InitialSceneLayout: "A rectangular table (80cm x 120cm) in the center of the kitchen with a sink on the left side. The robot arm is positioned at the edge of the table.",
		Skills:             []string{"pick", "place", "navigate"},
		SOPID:              "sop_dish_cleaning_v2",
		Topics:             []string{"/camera/color/image_raw", "/camera/depth/image_rect_raw", "/joint_states", "/gripper/state", "/odom"},
		StartCallbackURL:   "http://keystone.factory.internal/api/v1/callbacks/start",
		FinishCallbackURL:  "http://keystone.factory.internal/api/v1/callbacks/finish",
		UserToken:          "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyXzAwMSIsInNjb3BlIjpbImRldmljZSJdLCJleHAiOjE3MzY4MTIwMDB9.mock_signature",
		RecordingConfig: RecordingConfig{
			MaxDurationSec: 600,
			Compression:    "zstd",
		},
	}

	c.JSON(http.StatusOK, taskConfig)
}
