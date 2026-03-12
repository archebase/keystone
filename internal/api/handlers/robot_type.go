// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// RobotTypeHandler handles robot type related HTTP requests.
type RobotTypeHandler struct {
	db *sqlx.DB
}

// NewRobotTypeHandler creates a new RobotTypeHandler.
func NewRobotTypeHandler(db *sqlx.DB) *RobotTypeHandler {
	return &RobotTypeHandler{db: db}
}

// CreateRobotTypeRequest represents the request body for creating a robot type.
type CreateRobotTypeRequest struct {
	Name      string   `json:"name"`
	Model     string   `json:"model"`
	ROSTopics []string `json:"ros_topics"`
}

// CreateRobotTypeResponse represents the response body for creating a robot type.
type CreateRobotTypeResponse struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Model     string   `json:"model"`
	ROSTopics []string `json:"ros_topics"`
	CreatedAt string   `json:"created_at"`
}

// RegisterRoutes registers robot type related routes.
func (h *RobotTypeHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/robot_types", h.CreateRobotType)
}

// CreateRobotType handles robot type creation requests.
//
// @Summary      Create robot type
// @Description  Creates a new robot type with only required fields
// @Tags         robot_types
// @Accept       json
// @Produce      json
// @Param        body  body      CreateRobotTypeRequest   true  "Robot type payload"
// @Success      201   {object}  CreateRobotTypeResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /robot_types [post]
func (h *RobotTypeHandler) CreateRobotType(c *gin.Context) {
	var req CreateRobotTypeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Model = strings.TrimSpace(req.Model)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}

	if len(req.ROSTopics) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ros_topics is required"})
		return
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO robot_types (
			name,
			model,
			ros_topics,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?)`,
		req.Name,
		req.Model,
		toNullableJSONArray(req.ROSTopics),
		now,
		now,
	)
	if err != nil {
		log.Printf("[CreateRobotType] Failed to insert robot type: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot type"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		log.Printf("[CreateRobotType] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot type"})
		return
	}

	c.JSON(http.StatusCreated, CreateRobotTypeResponse{
		ID:        id,
		Name:      req.Name,
		Model:     req.Model,
		ROSTopics: req.ROSTopics,
		CreatedAt: now.Format(time.RFC3339),
	})
}

func toNullableJSONArray(values []string) sql.NullString {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	if len(cleaned) == 0 {
		return sql.NullString{}
	}

	encoded := "[\"" + strings.Join(cleaned, "\",\"") + "\"]"
	return sql.NullString{String: encoded, Valid: true}
}
