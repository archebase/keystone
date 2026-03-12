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
)

// RobotHandler handles robot related HTTP requests.
type RobotHandler struct {
	db *sqlx.DB
}

// NewRobotHandler creates a new RobotHandler.
func NewRobotHandler(db *sqlx.DB) *RobotHandler {
	return &RobotHandler{db: db}
}

// RobotResponse represents a robot in the response.
type RobotResponse struct {
	ID          string `json:"id"`
	RobotTypeID string `json:"robot_type_id"`
	DeviceID    string `json:"device_id"`
	FactoryID   string `json:"factory_id"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// RobotListResponse represents the response for listing robots.
type RobotListResponse struct {
	Robots []RobotResponse `json:"robots"`
}

// CreateRobotRequest represents the request body for creating a robot.
type CreateRobotRequest struct {
	RobotTypeID string `json:"robot_type_id"`
	DeviceID    string `json:"device_id"`
	FactoryID   string `json:"factory_id"`
}

// CreateRobotResponse represents the response for creating a robot.
type CreateRobotResponse struct {
	ID          string `json:"id"`
	RobotTypeID string `json:"robot_type_id"`
	DeviceID    string `json:"device_id"`
	FactoryID   string `json:"factory_id"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// RegisterRoutes registers robot related routes.
func (h *RobotHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robots", h.ListRobots)
	apiV1.POST("/robots", h.CreateRobot)
}

// robotRow represents a robot in the database
type robotRow struct {
	ID          int64          `db:"id"`
	RobotTypeID int64          `db:"robot_type_id"`
	DeviceID    string         `db:"device_id"`
	FactoryID   int64          `db:"factory_id"`
	Status      string         `db:"status"`
	CreatedAt   sql.NullString `db:"created_at"`
	FactorySlug string         `db:"factory_slug"`
}

// ListRobots handles robot listing requests with filtering.
//
// @Summary      List robots
// @Description  Lists robots with optional filtering by factory_id, status, and robot_type_id
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        factory_id    query     string  false  "Filter by factory slug (e.g., factory_shanghai)"
// @Param        status        query     string  false  "Filter by status (active, maintenance, retired)"
// @Param        robot_type_id query     string  false  "Filter by robot type slug"
// @Success      200           {object}  RobotListResponse
// @Failure      500           {object}  map[string]string
// @Router       /robots [get]
func (h *RobotHandler) ListRobots(c *gin.Context) {
	factoryID := c.Query("factory_id")
	status := c.Query("status")
	robotTypeID := c.Query("robot_type_id")

	// Build query with optional filters
	query := `
		SELECT 
			r.id,
			r.robot_type_id,
			r.device_id,
			r.factory_id,
			r.status,
			r.created_at
		FROM robots r
		WHERE r.deleted_at IS NULL
	`
	args := []interface{}{}

	if factoryID != "" {
		query += " AND r.factory_id = ?"
		args = append(args, factoryID)
	}

	if status != "" {
		query += " AND r.status = ?"
		args = append(args, status)
	}

	if robotTypeID != "" {
		// Parse robot_type_id as numeric value
		parsedRobotTypeID, err := strconv.ParseInt(robotTypeID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
			return
		}
		query += " AND r.robot_type_id = ?"
		args = append(args, parsedRobotTypeID)
	}

	query += " ORDER BY r.id DESC"

	// Use db.Select for cleaner code and automatic resource management
	var dbRows []robotRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		log.Printf("[ListRobots] Failed to query robots: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robots"})
		return
	}

	robots := []RobotResponse{}
	for _, r := range dbRows {
		robots = append(robots, RobotResponse{
			ID:          fmt.Sprintf("%d", r.ID),
			RobotTypeID: fmt.Sprintf("%d", r.RobotTypeID),
			DeviceID:    r.DeviceID,
			FactoryID:   fmt.Sprintf("%d", r.FactoryID),
			Status:      r.Status,
		})
	}

	c.JSON(http.StatusOK, RobotListResponse{
		Robots: robots,
	})
}

// CreateRobot handles robot creation requests.
//
// @Summary      Create robot
// @Description  Registers a new robot
// @Tags         robots
// @Accept       json
// @Produce      json
// @Param        body  body      CreateRobotRequest  true  "Robot payload"
// @Success      201   {object}  CreateRobotResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /robots [post]
func (h *RobotHandler) CreateRobot(c *gin.Context) {
	var req CreateRobotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.RobotTypeID = strings.TrimSpace(req.RobotTypeID)
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.FactoryID = strings.TrimSpace(req.FactoryID)

	if req.RobotTypeID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type_id is required"})
		return
	}

	if req.DeviceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "device_id is required"})
		return
	}

	if req.FactoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id is required"})
		return
	}

	// Parse robot_type_id as numeric value
	robotTypeID, err := strconv.ParseInt(req.RobotTypeID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
		return
	}
	log.Printf("[CreateRobot] Parsed robot_type_id: %d", robotTypeID)

	// Verify robot_type exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", robotTypeID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type not found"})
		return
	}

	// Parse factory_id as direct number
	factoryID, err := strconv.ParseInt(req.FactoryID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
		return
	}
	log.Printf("[CreateRobot] Parsed factory_id: %d", factoryID)

	// Verify factory exists
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
		return
	}

	// Generate created_at timestamp in application layer
	createdAt := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Insert the robot
	result, err := h.db.Exec(
		`INSERT INTO robots (
			robot_type_id,
			device_id,
			factory_id,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		robotTypeID,
		req.DeviceID,
		factoryID,
		"active",
		createdAt,
		createdAt,
	)
	if err != nil {
		log.Printf("[CreateRobot] Failed to insert robot: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		log.Printf("[CreateRobot] Failed to get inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create robot"})
		return
	}

	c.JSON(http.StatusCreated, CreateRobotResponse{
		ID:          fmt.Sprintf("%d", id),
		RobotTypeID: req.RobotTypeID,
		DeviceID:    req.DeviceID,
		FactoryID:   req.FactoryID,
		Status:      "active",
		CreatedAt:   createdAt,
	})
}
