// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
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
	Email      string `json:"email,omitempty"`
}

// CreateDataCollectorResponse represents the response for creating a data collector.
type CreateDataCollectorResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	OperatorID string `json:"operator_id"`
	Email      string `json:"email,omitempty"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// RegisterRoutes registers data collector related routes.
func (h *DataCollectorHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/data_collectors", h.ListDataCollectors)
	apiV1.POST("/data_collectors", h.CreateDataCollector)
	apiV1.GET("/data_collectors/:id", h.GetDataCollector)
	apiV1.PUT("/data_collectors/:id", h.UpdateDataCollector)
	apiV1.DELETE("/data_collectors/:id", h.DeleteDataCollector)
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
		logger.Printf("[DC] Failed to query data collectors: %v", err)
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
	req.Email = strings.TrimSpace(req.Email)

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
		logger.Printf("[DC] Failed to check operator_id: %v", err)
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
	var emailStr sql.NullString
	if req.Email != "" {
		emailStr = sql.NullString{String: req.Email, Valid: true}
	}

	result, err := h.db.Exec(
		`INSERT INTO data_collectors (
			name,
			operator_id,
			email,
			status,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		req.Name,
		req.OperatorID,
		emailStr,
		"active",
		createdAt,
		createdAt,
	)
	if err != nil {
		logger.Printf("[DC] Failed to insert data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[DC] Failed to get inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}

	c.JSON(http.StatusCreated, CreateDataCollectorResponse{
		ID:         fmt.Sprintf("%d", id),
		Name:       req.Name,
		OperatorID: req.OperatorID,
		Email:      req.Email,
		Status:     "active",
		CreatedAt:  createdAt,
	})
}

// GetDataCollector handles getting a single data collector by ID.
//
// @Summary      Get data collector
// @Description  Gets a data collector by ID
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Data Collector ID"
// @Success      200  {object}  DataCollectorResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /data_collectors/{id} [get]
func (h *DataCollectorHandler) GetDataCollector(c *gin.Context) {
	idParam := c.Param("id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data collector id"})
		return
	}

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
		WHERE dc.id = ? AND dc.deleted_at IS NULL
	`

	var dc dataCollectorRow
	if err := h.db.Get(&dc, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
			return
		}
		logger.Printf("[DC] Failed to query data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data collector"})
		return
	}

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

	c.JSON(http.StatusOK, DataCollectorResponse{
		ID:            fmt.Sprintf("%d", dc.ID),
		Name:          dc.Name,
		OperatorID:    dc.OperatorID,
		Email:         email,
		Certification: certification,
		Status:        dc.Status,
		CreatedAt:     createdAt,
	})
}

// UpdateDataCollectorRequest represents the request body for updating a data collector.
type UpdateDataCollectorRequest struct {
	Name   *string `json:"name,omitempty"`
	Email  *string `json:"email,omitempty"`
	Status *string `json:"status,omitempty"`
}

// UpdateDataCollector handles updating a data collector.
//
// @Summary      Update data collector
// @Description  Updates an existing data collector
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        id   path      string                  true  "Data Collector ID"
// @Param        body body      UpdateDataCollectorRequest  true  "Data Collector payload"
// @Success      200  {object}  DataCollectorResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /data_collectors/{id} [put]
func (h *DataCollectorHandler) UpdateDataCollector(c *gin.Context) {
	idParam := c.Param("id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data collector id"})
		return
	}

	var req UpdateDataCollectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	validStatuses := map[string]bool{
		"active":   true,
		"inactive": true,
		"on_leave": true,
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name != "" {
			updates = append(updates, "name = ?")
			args = append(args, name)
		}
	}

	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		var emailStr sql.NullString
		if email != "" {
			emailStr = sql.NullString{String: email, Valid: true}
		}
		updates = append(updates, "email = ?")
		args = append(args, emailStr)
	}

	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if !validStatuses[status] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status, must be one of: active, inactive, on_leave"})
			return
		}
		updates = append(updates, "status = ?")
		args = append(args, status)
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	updatedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	updates = append(updates, "updated_at = ?")
	args = append(args, updatedAt)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE data_collectors SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	result, err := h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[DC] Failed to update data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
		return
	}

	// Fetch the updated data collector
	var dc dataCollectorRow
	err = h.db.Get(&dc, `
		SELECT 
			dc.id,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.created_at
		FROM data_collectors dc
		WHERE dc.id = ?
	`, id)
	if err != nil {
		logger.Printf("[DC] Failed to fetch updated data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated data collector"})
		return
	}

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

	c.JSON(http.StatusOK, DataCollectorResponse{
		ID:            fmt.Sprintf("%d", dc.ID),
		Name:          dc.Name,
		OperatorID:    dc.OperatorID,
		Email:         email,
		Certification: certification,
		Status:        dc.Status,
		CreatedAt:     createdAt,
	})
}

// DeleteDataCollector handles data collector deletion requests (soft delete).
//
// @Summary      Delete data collector
// @Description  Soft deletes a data collector by ID
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Data Collector ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors/{id} [delete]
func (h *DataCollectorHandler) DeleteDataCollector(c *gin.Context) {
	idParam := c.Param("id")
	id, err := strconv.ParseInt(idParam, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data collector id"})
		return
	}

	// Check if data collector exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM data_collectors WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[DC] Failed to check data collector existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
		return
	}

	updatedAt := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE data_collectors SET deleted_at = NOW(), updated_at = ? WHERE id = ?", updatedAt, id)
	if err != nil {
		logger.Printf("[DC] Failed to delete data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}

	c.Status(http.StatusNoContent)
}
