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

// DataCollectorHandler handles data collector related HTTP requests.
type DataCollectorHandler struct {
	db *sqlx.DB
}

// NewDataCollectorHandler creates a new DataCollectorHandler.
func NewDataCollectorHandler(db *sqlx.DB) *DataCollectorHandler {
	return &DataCollectorHandler{db: db}
}

// DataCollectorResponse represents a data collector in the response.
type DataCollectorResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OperatorID    string `json:"operator_id"`
	Email         string `json:"email,omitempty"`
	Certification string `json:"certification,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at,omitempty"`
}

// DataCollectorListResponse represents the response for listing data collectors.
type DataCollectorListResponse struct {
	DataCollectors []DataCollectorResponse `json:"data_collectors"`
}

// CreateDataCollectorRequest represents the request body for creating a data collector.
type CreateDataCollectorRequest struct {
	Name       string `json:"name"`
	OperatorID string `json:"operator_id"`
}

// CreateDataCollectorResponse represents the response for creating a data collector.
type CreateDataCollectorResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	OperatorID string `json:"operator_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// RegisterRoutes registers data collector related routes.
func (h *DataCollectorHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/data_collectors", h.ListDataCollectors)
	apiV1.POST("/data_collectors", h.CreateDataCollector)
	apiV1.PATCH("/data_collectors/:id/status", h.UpdateDataCollectorStatus)
}

// dataCollectorRow represents a data collector in the database
type dataCollectorRow struct {
	ID            int64          `db:"id"`
	Name          string         `db:"name"`
	OperatorID    string         `db:"operator_id"`
	Email         sql.NullString `db:"email"`
	Certification sql.NullString `db:"certification"`
	Status        string         `db:"status"`
	CreatedAt     sql.NullString `db:"created_at"`
}

// ListDataCollectors handles data collector listing requests with filtering.
//
// @Summary      List data collectors
// @Description  Lists data collectors with optional filtering by status
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        status  query     string  false  "Filter by status (active, inactive, on_leave)"
// @Success      200     {object}  DataCollectorListResponse
// @Failure      500     {object}  map[string]string
// @Router       /data_collectors [get]
func (h *DataCollectorHandler) ListDataCollectors(c *gin.Context) {
	status := c.Query("status")

	// Build query with optional filters
	query := `
		SELECT 
			dc.id,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.created_at
		FROM data_collectors dc
		WHERE dc.deleted_at IS NULL
	`
	args := []interface{}{}

	if status != "" {
		query += " AND dc.status = ?"
		args = append(args, status)
	}

	query += " ORDER BY dc.id DESC"

	// Use db.Select for cleaner code and automatic resource management
	var dbRows []dataCollectorRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		log.Printf("[ListDataCollectors] Failed to query data collectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}

	dataCollectors := []DataCollectorResponse{}
	for _, dc := range dbRows {
		email := ""
		if dc.Email.Valid {
			email = dc.Email.String
		}

		certification := ""
		if dc.Certification.Valid {
			certification = dc.Certification.String
		}

		createdAt := ""
		if dc.CreatedAt.Valid {
			createdAt = dc.CreatedAt.String
		}

		dataCollectors = append(dataCollectors, DataCollectorResponse{
			ID:            fmt.Sprintf("%d", dc.ID),
			Name:          dc.Name,
			OperatorID:    dc.OperatorID,
			Email:         email,
			Certification: certification,
			Status:        dc.Status,
			CreatedAt:     createdAt,
		})
	}

	c.JSON(http.StatusOK, DataCollectorListResponse{
		DataCollectors: dataCollectors,
	})
}

// CreateDataCollector handles data collector creation requests.
//
// @Summary      Create data collector
// @Description  Registers a new data collector
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        body  body      CreateDataCollectorRequest  true  "Data collector payload"
// @Success      201   {object}  CreateDataCollectorResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /data_collectors [post]
func (h *DataCollectorHandler) CreateDataCollector(c *gin.Context) {
	var req CreateDataCollectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.OperatorID = strings.TrimSpace(req.OperatorID)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.OperatorID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id is required"})
		return
	}

	// Check if operator_id already exists
	var exists bool
	err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM data_collectors WHERE operator_id = ? AND deleted_at IS NULL)", req.OperatorID)
	if err != nil {
		log.Printf("[CreateDataCollector] Failed to check operator_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	if exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id already exists"})
		return
	}

	// Generate created_at timestamp in application layer
	createdAt := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Insert the data collector
	result, err := h.db.Exec(
		`INSERT INTO data_collectors (
			name,
			operator_id,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?)`,
		req.Name,
		req.OperatorID,
		"active",
		createdAt,
		createdAt,
	)
	if err != nil {
		log.Printf("[CreateDataCollector] Failed to insert data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		log.Printf("[CreateDataCollector] Failed to get inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}

	c.JSON(http.StatusCreated, CreateDataCollectorResponse{
		ID:         fmt.Sprintf("%d", id),
		Name:       req.Name,
		OperatorID: req.OperatorID,
		Status:     "active",
		CreatedAt:  createdAt,
	})
}

// UpdateDataCollectorStatusRequest represents the request body for updating data collector status.
type UpdateDataCollectorStatusRequest struct {
	Status string `json:"status"`
}

// UpdateDataCollectorStatusResponse represents the response for updating data collector status.
type UpdateDataCollectorStatusResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// UpdateDataCollectorStatus handles status update requests for a data collector.
//
// @Summary      Update data collector status
// @Description  Updates the status of an existing data collector
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        id   path      string                         true  "Data collector ID"
// @Param        body body      UpdateDataCollectorStatusRequest  true  "Status payload"
// @Success      200 {object}  UpdateDataCollectorStatusResponse
// @Failure      400 {object}  map[string]string
// @Failure      404 {object}  map[string]string
// @Failure      500 {object}  map[string]string
// @Router       /data_collectors/{id}/status [patch]
func (h *DataCollectorHandler) UpdateDataCollectorStatus(c *gin.Context) {
	idParam := c.Param("id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data collector id"})
		return
	}

	var req UpdateDataCollectorStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Status = strings.TrimSpace(req.Status)

	// Validate status value
	validStatuses := map[string]bool{
		"active":   true,
		"inactive": true,
		"on_leave": true,
	}
	if !validStatuses[req.Status] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status, must be one of: active, inactive, on_leave"})
		return
	}

	// Check if data collector exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM data_collectors WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		log.Printf("[UpdateDataCollectorStatus] Failed to check data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update status"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
		return
	}

	// Generate updated_at timestamp
	updatedAt := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Update the status
	_, err = h.db.Exec(
		`UPDATE data_collectors SET status = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		req.Status,
		updatedAt,
		id,
	)
	if err != nil {
		log.Printf("[UpdateDataCollectorStatus] Failed to update status: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update status"})
		return
	}

	c.JSON(http.StatusOK, UpdateDataCollectorStatusResponse{
		ID:     idParam,
		Status: req.Status,
	})
}
