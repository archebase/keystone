// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// TaskHandler handles task-related HTTP requests
type TaskHandler struct {
	db *sqlx.DB
}

// NewTaskHandler creates a new TaskHandler
func NewTaskHandler(db *sqlx.DB) *TaskHandler {
	return &TaskHandler{
		db: db,
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
	CallbackURLs       CallbackURLs    `json:"callback_urls"`
	UserToken          string          `json:"user_token"`
	RecordingConfig    RecordingConfig `json:"recording_config"`
}

// RecordingConfig represents recording configuration settings
type RecordingConfig struct {
	MaxDurationSec int    `json:"max_duration_sec"`
	Compression    string `json:"compression"`
}

// CallbackURLs represents callback URLs for task events
type CallbackURLs struct {
	Start  string `json:"start"`
	Finish string `json:"finish"`
}

// RegisterRoutes registers task-related routes
func (h *TaskHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/tasks", h.CreateTask)
	apiV1.GET("/tasks/:id/config", h.GetTaskConfig)
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
// It sets up POST /start endpoint to handle recording start callbacks.
func (h *TaskHandler) RegisterCallbackRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/start", h.OnRecordingStart)
}

// RecordingStartCallback represents the callback payload from axon recorder
type RecordingStartCallback struct {
	TaskID    string   `json:"task_id"`
	DeviceID  string   `json:"device_id"`
	Status    string   `json:"status"`
	StartedAt string   `json:"started_at"`
	Topics    []string `json:"topics"`
}

// OnRecordingStart handles callback from axon recorder when recording starts.
// @Summary      Recording start callback
// @Description  Handles callback from axon recorder when recording starts, updates task status to in_progress if current status is ready
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
		log.Printf("[OnRecordingStart] Failed to parse request body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Invalid request body: " + err.Error(),
		})
		return
	}

	log.Printf("[OnRecordingStart] Received start callback: task_id=%s, device_id=%s",
		callback.TaskID, callback.DeviceID)

	// Validate required fields
	if callback.TaskID == "" {
		log.Printf("[OnRecordingStart] Missing task_id in callback")
		c.JSON(http.StatusBadRequest, gin.H{
			"error_msg": "Missing required field: task_id",
		})
		return
	}

	// Query the database to check current task status
	var row struct {
		Status string `db:"status"`
	}
	err := h.db.Get(&row,
		"SELECT status FROM tasks WHERE task_id = ? AND deleted_at IS NULL",
		callback.TaskID,
	)

	if err == sql.ErrNoRows {
		log.Printf("[OnRecordingStart] Task not found: task_id=%s", callback.TaskID)
		c.JSON(http.StatusNotFound, gin.H{
			"error_msg": "Task not found",
		})
		return
	}

	if err != nil {
		log.Printf("[OnRecordingStart] Failed to query task status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error_msg": "Failed to query task status",
		})
		return
	}

	// Check if task status is 'ready'
	if row.Status != "ready" {
		log.Printf("[OnRecordingStart] Task not in 'ready' state: task_id=%s, status=%s",
			callback.TaskID, row.Status)
		c.JSON(http.StatusConflict, gin.H{
			"error_msg": "Task is not in 'ready' state, current status: " + row.Status,
		})
		return
	}

	// Update task status to 'in_progress'
	now := time.Now()
	result, err := h.db.Exec(
		"UPDATE tasks SET status = 'in_progress', updated_at = ? WHERE task_id = ? AND status = 'ready' AND deleted_at IS NULL",
		now, callback.TaskID,
	)

	if err != nil {
		log.Printf("[OnRecordingStart] Failed to update task status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error_msg": "Failed to update task status",
		})
		return
	}

	// Check if any row was updated
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		log.Printf("[OnRecordingStart] Failed to get rows affected: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error_msg": "Failed to verify update",
		})
		return
	}

	if rowsAffected == 0 {
		log.Printf("[OnRecordingStart] No rows updated (concurrent modification): task_id=%s", callback.TaskID)
		c.JSON(http.StatusConflict, gin.H{
			"error_msg": "Task status changed concurrently",
		})
		return
	}

	log.Printf("[OnRecordingStart] Successfully updated task status to 'in_progress': task_id=%s", callback.TaskID)

	nowStr := now.Format(time.RFC3339)
	c.JSON(http.StatusOK, gin.H{
		"status":          "acknowledged",
		"task_status":     "in_progress",
		"acknowledged_at": nowStr,
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
// @Router       /tasks/{id}/config [get]
func (h *TaskHandler) GetTaskConfig(c *gin.Context) {
	taskID := c.Param("id")

	// Query the database to check task status
	var statusRow struct {
		Status string `db:"status"`
	}
	err := h.db.Get(&statusRow,
		"SELECT status FROM tasks WHERE task_id = ? AND deleted_at IS NULL",
		taskID,
	)

	if err == sql.ErrNoRows {
		// Task not found
		c.JSON(http.StatusNotFound, gin.H{
			"error": "task not found",
		})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "failed to query task",
		})
		return
	}

	// Check if task status is 'ready'
	if statusRow.Status != "ready" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "task not in 'ready' state",
		})
		return
	}

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
		CallbackURLs: CallbackURLs{
			Start:  "https://keystone.factory.internal/api/v1/callbacks/start",
			Finish: "https://keystone.factory.internal/api/v1/callbacks/finish",
		},
		UserToken: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyXzAwMSIsInNjb3BlIjpbImRldmljZSJdLCJleHAiOjE3MzY4MTIwMDB9.mock_signature",
		RecordingConfig: RecordingConfig{
			MaxDurationSec: 600,
			Compression:    "zstd",
		},
	}

	c.JSON(http.StatusOK, taskConfig)
}
