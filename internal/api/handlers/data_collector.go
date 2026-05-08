// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"

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
	ID               string      `json:"id"`
	OrganizationID   string      `json:"organization_id"`
	OrganizationName string      `json:"organization_name,omitempty"`
	Name             string      `json:"name"`
	OperatorID       string      `json:"operator_id"`
	Email            string      `json:"email,omitempty"`
	Certification    string      `json:"certification,omitempty"`
	Status           string      `json:"status"`
	Metadata         interface{} `json:"metadata,omitempty"`
	CreatedAt        string      `json:"created_at,omitempty"`
	UpdatedAt        string      `json:"updated_at,omitempty"`
}

// DataCollectorListResponse represents the response for listing data collectors.
type DataCollectorListResponse struct {
	Items   []DataCollectorResponse `json:"items"`
	Total   int                     `json:"total"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	HasNext bool                    `json:"hasNext,omitempty"`
	HasPrev bool                    `json:"hasPrev,omitempty"`
}

// CreateDataCollectorRequest represents the request body for creating a data collector.
type CreateDataCollectorRequest struct {
	OrganizationID string      `json:"organization_id"`
	Name           string      `json:"name"`
	OperatorID     string      `json:"operator_id"`
	Email          string      `json:"email,omitempty"`
	Certification  string      `json:"certification,omitempty"`
	Password       string      `json:"password,omitempty"` // #nosec G117 -- request DTO may include password for initial set
	Metadata       interface{} `json:"metadata,omitempty"`
}

// CreateDataCollectorResponse represents the response for creating a data collector.
type CreateDataCollectorResponse struct {
	ID               string      `json:"id"`
	OrganizationID   string      `json:"organization_id"`
	OrganizationName string      `json:"organization_name,omitempty"`
	Name             string      `json:"name"`
	OperatorID       string      `json:"operator_id"`
	Email            string      `json:"email,omitempty"`
	Certification    string      `json:"certification,omitempty"`
	Status           string      `json:"status"`
	Metadata         interface{} `json:"metadata,omitempty"`
	CreatedAt        string      `json:"created_at"`
	UpdatedAt        string      `json:"updated_at,omitempty"`
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
	ID               int64          `db:"id"`
	OrganizationID   int64          `db:"organization_id"`
	OrganizationName sql.NullString `db:"organization_name"`
	Name             string         `db:"name"`
	OperatorID       string         `db:"operator_id"`
	Email            sql.NullString `db:"email"`
	Certification    sql.NullString `db:"certification"`
	Status           string         `db:"status"`
	Metadata         sql.NullString `db:"metadata"`
	CreatedAt        sql.NullTime   `db:"created_at"`
	UpdatedAt        sql.NullTime   `db:"updated_at"`
}

func dcMetadataFromDB(ns sql.NullString) interface{} {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	return parseJSONRaw(ns.String)
}

func dataCollectorResponseFromRow(dc dataCollectorRow) DataCollectorResponse {
	email := ""
	if dc.Email.Valid {
		email = dc.Email.String
	}
	orgName := ""
	if dc.OrganizationName.Valid {
		orgName = dc.OrganizationName.String
	}
	certification := ""
	if dc.Certification.Valid {
		certification = dc.Certification.String
	}
	createdAt := ""
	if dc.CreatedAt.Valid {
		createdAt = dc.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if dc.UpdatedAt.Valid {
		updatedAt = dc.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	return DataCollectorResponse{
		ID:               fmt.Sprintf("%d", dc.ID),
		OrganizationID:   fmt.Sprintf("%d", dc.OrganizationID),
		OrganizationName: orgName,
		Name:             dc.Name,
		OperatorID:       dc.OperatorID,
		Email:            email,
		Certification:    certification,
		Status:           dc.Status,
		Metadata:         dcMetadataFromDB(dc.Metadata),
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}
}

// ListDataCollectors handles data collector listing requests with filtering.
//
// @Summary      List data collectors
// @Description  Lists all data collectors with optional filtering and keyword search
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        organization_id query     string  false  "Filter by organization ID"
// @Param        status          query     string  false  "Filter by status (active, inactive, on_leave)"
// @Param        keyword         query     string  false  "Search by name, operator ID, or email"
// @Param        q               query     string  false  "Alias of keyword"
// @Param        search          query     string  false  "Alias of keyword"
// @Param        operator_id     query     string  false  "Alias of keyword for operator ID search"
// @Param        limit           query     int     false  "Max results (default 50, max 100)"
// @Param        offset          query     int     false  "Pagination offset (default 0)"
// @Success      200     {object}  DataCollectorListResponse
// @Failure      400     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /data_collectors [get]
func (h *DataCollectorHandler) ListDataCollectors(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	orgID := c.Query("organization_id")
	status := c.Query("status")
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search", "operator_id")

	whereClause := "WHERE dc.deleted_at IS NULL"
	args := []interface{}{}

	if orgID != "" {
		parsedOrgID, err := strconv.ParseInt(orgID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
			return
		}
		whereClause += " AND dc.organization_id = ?"
		args = append(args, parsedOrgID)
	}

	if status != "" {
		whereClause += " AND dc.status = ?"
		args = append(args, status)
	}

	if keyword != "" {
		likeKeyword := "%" + keyword + "%"
		whereClause += " AND (dc.name LIKE ? OR dc.operator_id LIKE ? OR dc.email LIKE ?)"
		args = append(args, likeKeyword, likeKeyword, likeKeyword)
	}

	countQuery := "SELECT COUNT(*) FROM data_collectors dc " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[DC] Failed to count data collectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}

	query := `
		SELECT 
			dc.id,
			dc.organization_id,
			o.name AS organization_name,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.metadata,
			dc.created_at,
			dc.updated_at
		FROM data_collectors dc
		INNER JOIN organizations o ON o.id = dc.organization_id AND o.deleted_at IS NULL
		` + whereClause + `
		ORDER BY dc.id DESC
		LIMIT ? OFFSET ?
	`

	queryArgs := append(args, pagination.Limit, pagination.Offset)

	var dbRows []dataCollectorRow
	if err := h.db.Select(&dbRows, query, queryArgs...); err != nil {
		logger.Printf("[DC] Failed to query data collectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}

	dataCollectors := []DataCollectorResponse{}
	for _, dc := range dbRows {
		dataCollectors = append(dataCollectors, dataCollectorResponseFromRow(dc))
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, DataCollectorListResponse{
		Items:   dataCollectors,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
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

	req.OrganizationID = strings.TrimSpace(req.OrganizationID)
	req.Name = strings.TrimSpace(req.Name)
	req.OperatorID = strings.TrimSpace(req.OperatorID)
	req.Email = strings.TrimSpace(req.Email)
	req.Certification = strings.TrimSpace(req.Certification)
	req.Password = strings.TrimSpace(req.Password)

	if req.OrganizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id is required"})
		return
	}

	orgID, err := strconv.ParseInt(req.OrganizationID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
		return
	}

	// Verify organization exists
	var orgExists bool
	if err := h.db.Get(&orgExists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id = ? AND deleted_at IS NULL)", orgID); err != nil || !orgExists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization not found"})
		return
	}

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
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM data_collectors WHERE operator_id = ? AND deleted_at IS NULL)", req.OperatorID); err != nil {
		logger.Printf("[DC] Failed to check operator_id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	if exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id already exists"})
		return
	}

	now := time.Now().UTC()

	// Insert the data collector
	var emailStr sql.NullString
	if req.Email != "" {
		emailStr = sql.NullString{String: req.Email, Valid: true}
	}

	var certStr sql.NullString
	if req.Certification != "" {
		certStr = sql.NullString{String: req.Certification, Valid: true}
	}

	metadataStr := sql.NullString{String: "{}", Valid: true}
	if req.Metadata != nil {
		metadataJSON, err := json.Marshal(req.Metadata)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
			return
		}
		metadataStr = sql.NullString{String: string(metadataJSON), Valid: true}
	}

	var passwordHash sql.NullString
	password := req.Password
	if password == "" {
		password = "123456"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		logger.Printf("[DC] Failed to hash password: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	passwordHash = sql.NullString{String: string(hash), Valid: true}

	result, err := h.db.Exec(
		`INSERT INTO data_collectors (
			organization_id,
			name,
			operator_id,
			email,
			password_hash,
			certification,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID,
		req.Name,
		req.OperatorID,
		emailStr,
		passwordHash,
		certStr,
		"active",
		metadataStr,
		now,
		now,
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

	var row dataCollectorRow
	err = h.db.Get(&row, `
		SELECT 
			dc.id,
			dc.organization_id,
			o.name AS organization_name,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.metadata,
			dc.created_at,
			dc.updated_at
		FROM data_collectors dc
		INNER JOIN organizations o ON o.id = dc.organization_id AND o.deleted_at IS NULL
		WHERE dc.id = ? AND dc.deleted_at IS NULL
	`, id)
	if err != nil {
		logger.Printf("[DC] Failed to load created data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}

	resp := dataCollectorResponseFromRow(row)
	c.JSON(http.StatusCreated, CreateDataCollectorResponse(resp))
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
			dc.organization_id,
			o.name AS organization_name,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.metadata,
			dc.created_at,
			dc.updated_at
		FROM data_collectors dc
		INNER JOIN organizations o ON o.id = dc.organization_id AND o.deleted_at IS NULL
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

	c.JSON(http.StatusOK, dataCollectorResponseFromRow(dc))
}

// UpdateDataCollectorRequest represents the request body for updating a data collector.
// Metadata uses json.RawMessage so we can tell: key omitted (no change) vs explicit JSON null (store {}).
type UpdateDataCollectorRequest struct {
	Name          *string         `json:"name,omitempty"`
	OperatorID    *string         `json:"operator_id,omitempty"`
	Email         *string         `json:"email,omitempty"`
	Certification *string         `json:"certification,omitempty"`
	Status        *string         `json:"status,omitempty"`
	Password      *string         `json:"password,omitempty"` // #nosec G117 -- request DTO may include password update
	Metadata      json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
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

	if req.OperatorID != nil {
		op := strings.TrimSpace(*req.OperatorID)
		if op == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id cannot be empty"})
			return
		}
		var taken bool
		err := h.db.Get(&taken, `SELECT EXISTS(
			SELECT 1 FROM data_collectors WHERE operator_id = ? AND deleted_at IS NULL AND id != ?
		)`, op, id)
		if err != nil {
			logger.Printf("[DC] Failed to check operator_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
			return
		}
		if taken {
			c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id already exists"})
			return
		}
		updates = append(updates, "operator_id = ?")
		args = append(args, op)
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

	if req.Certification != nil {
		cert := strings.TrimSpace(*req.Certification)
		var certStr sql.NullString
		if cert != "" {
			certStr = sql.NullString{String: cert, Valid: true}
		}
		updates = append(updates, "certification = ?")
		args = append(args, certStr)
	}

	if req.Password != nil {
		pw := strings.TrimSpace(*req.Password)
		if pw == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "password cannot be empty"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			logger.Printf("[DC] Failed to hash password: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
			return
		}
		updates = append(updates, "password_hash = ?")
		args = append(args, sql.NullString{String: string(hash), Valid: true})
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

	if len(req.Metadata) > 0 {
		meta := bytes.TrimSpace(req.Metadata)
		if bytes.Equal(meta, []byte("null")) {
			updates = append(updates, "metadata = ?")
			args = append(args, sql.NullString{String: "{}", Valid: true})
		} else {
			var probe interface{}
			if err := json.Unmarshal(req.Metadata, &probe); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid metadata JSON"})
				return
			}
			updates = append(updates, "metadata = ?")
			args = append(args, sql.NullString{String: string(req.Metadata), Valid: true})
		}
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	updates = append(updates, "updated_at = ?")
	args = append(args, time.Now().UTC())
	args = append(args, id)

	query := fmt.Sprintf("UPDATE data_collectors SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", ")) //nolint:gosec // columns are hardcoded literals, not user input

	// Determine whether denormalized workstation columns need syncing.
	needsNameSync := req.Name != nil && strings.TrimSpace(*req.Name) != ""
	needsOpIDSync := req.OperatorID != nil && strings.TrimSpace(*req.OperatorID) != ""

	tx, err := h.db.Begin()
	if err != nil {
		logger.Printf("[DC] Failed to begin transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
		return
	}
	defer tx.Rollback() //nolint:errcheck

	result, err := tx.Exec(query, args...)
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

	// Sync denormalized columns in workstations if name or operator_id changed.
	if needsNameSync || needsOpIDSync {
		wsUpdates := []string{}
		wsArgs := []interface{}{}
		if needsNameSync {
			wsUpdates = append(wsUpdates, "collector_name = ?")
			wsArgs = append(wsArgs, strings.TrimSpace(*req.Name))
		}
		if needsOpIDSync {
			wsUpdates = append(wsUpdates, "collector_operator_id = ?")
			wsArgs = append(wsArgs, strings.TrimSpace(*req.OperatorID))
		}
		wsArgs = append(wsArgs, id)
		wsQuery := fmt.Sprintf( //nolint:gosec // columns are hardcoded literals, not user input
			"UPDATE workstations SET %s WHERE data_collector_id = ? AND deleted_at IS NULL",
			strings.Join(wsUpdates, ", "),
		)
		if _, err := tx.Exec(wsQuery, wsArgs...); err != nil {
			logger.Printf("[DC] Failed to sync workstation denormalized columns: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[DC] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
		return
	}

	// Fetch the updated data collector
	var dc dataCollectorRow
	err = h.db.Get(&dc, `
		SELECT
			dc.id,
			dc.organization_id,
			o.name AS organization_name,
			dc.name,
			dc.operator_id,
			dc.email,
			dc.certification,
			dc.status,
			dc.metadata,
			dc.created_at,
			dc.updated_at
		FROM data_collectors dc
		INNER JOIN organizations o ON o.id = dc.organization_id AND o.deleted_at IS NULL
		WHERE dc.id = ? AND dc.deleted_at IS NULL
	`, id)
	if err != nil {
		logger.Printf("[DC] Failed to fetch updated data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated data collector"})
		return
	}

	c.JSON(http.StatusOK, dataCollectorResponseFromRow(dc))
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
// @Failure      409 {object} map[string]string
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

	var usedByStation bool
	err = h.db.Get(&usedByStation, "SELECT EXISTS(SELECT 1 FROM workstations WHERE data_collector_id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[DC] Failed to check workstations referencing data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}
	if usedByStation {
		c.JSON(http.StatusConflict, gin.H{"error": "data collector is assigned to one or more workstations"})
		return
	}

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE data_collectors SET deleted_at = NOW(), updated_at = ? WHERE id = ? AND deleted_at IS NULL", time.Now().UTC(), id)
	if err != nil {
		logger.Printf("[DC] Failed to delete data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}

	c.Status(http.StatusNoContent)
}
