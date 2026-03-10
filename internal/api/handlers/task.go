// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
)

// TaskHandler handles task-related HTTP requests
type TaskHandler struct {
	db *sql.DB
}

// NewTaskHandler creates a new TaskHandler
func NewTaskHandler(db *sql.DB) *TaskHandler {
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
	apiV1.GET("/tasks/:id/config", h.GetTaskConfig)
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
	var status string
	err := h.db.QueryRow(
		"SELECT status FROM tasks WHERE task_id = ? AND deleted_at IS NULL",
		taskID,
	).Scan(&status)

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
	if status != "ready" {
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
