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

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// InspectorHandler handles inspector related HTTP requests.
type InspectorHandler struct {
	db *sqlx.DB
}

// NewInspectorHandler creates a new InspectorHandler.
func NewInspectorHandler(db *sqlx.DB) *InspectorHandler {
	return &InspectorHandler{db: db}
}

// InspectorResponse represents an inspector in the response.
type InspectorResponse struct {
	ID                 string      `json:"id"`
	Name               string      `json:"name"`
	InspectorID        string      `json:"inspector_id"`
	Email              string      `json:"email,omitempty"`
	CertificationLevel string      `json:"certification_level"`
	Status             string      `json:"status"`
	Metadata           interface{} `json:"metadata,omitempty"`
	CreatedAt          string      `json:"created_at,omitempty"`
	UpdatedAt          string      `json:"updated_at,omitempty"`
}

// InspectorListResponse represents the response for listing inspectors.
type InspectorListResponse struct {
	Inspectors []InspectorResponse `json:"inspectors"`
}

// CreateInspectorRequest represents the request body for creating an inspector.
type CreateInspectorRequest struct {
	Name               string      `json:"name"`
	InspectorID        string      `json:"inspector_id"`
	Email              string      `json:"email,omitempty"`
	CertificationLevel string      `json:"certification_level,omitempty"`
	Metadata           interface{} `json:"metadata,omitempty"`
}

// CreateInspectorResponse represents the response for creating an inspector.
type CreateInspectorResponse struct {
	ID                 string      `json:"id"`
	Name               string      `json:"name"`
	InspectorID        string      `json:"inspector_id"`
	CertificationLevel string      `json:"certification_level"`
	Status             string      `json:"status"`
	Metadata           interface{} `json:"metadata,omitempty"`
	CreatedAt          string      `json:"created_at"`
}

// UpdateInspectorRequest represents the request body for updating an inspector.
// Metadata uses json.RawMessage so callers can distinguish omitted key vs explicit JSON null (stored as {}).
type UpdateInspectorRequest struct {
	InspectorID        *string         `json:"inspector_id,omitempty"`
	Name               *string         `json:"name,omitempty"`
	Email              *string         `json:"email,omitempty"`
	CertificationLevel *string         `json:"certification_level,omitempty"`
	Status             *string         `json:"status,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// RegisterRoutes registers inspector related routes.
func (h *InspectorHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/inspectors", h.ListInspectors)
	apiV1.POST("/inspectors", h.CreateInspector)
	apiV1.GET("/inspectors/:id", h.GetInspector)
	apiV1.PUT("/inspectors/:id", h.UpdateInspector)
	apiV1.DELETE("/inspectors/:id", h.DeleteInspector)
}

// inspectorRow represents an inspector in the database
type inspectorRow struct {
	ID                 int64          `db:"id"`
	Name               string         `db:"name"`
	InspectorID        string         `db:"inspector_id"`
	Email              sql.NullString `db:"email"`
	CertificationLevel string         `db:"certification_level"`
	Status             string         `db:"status"`
	Metadata           sql.NullString `db:"metadata"`
	CreatedAt          sql.NullTime   `db:"created_at"`
	UpdatedAt          sql.NullTime   `db:"updated_at"`
}

// ListInspectors handles inspector listing requests.
//
// @Summary      List inspectors
// @Description  Lists all inspectors
// @Tags         inspectors
// @Accept       json
// @Produce      json
// @Param        status query string false "Filter by status (active, inactive)"
// @Success      200 {object} InspectorListResponse
// @Failure      500 {object} map[string]string
// @Router       /inspectors [get]
func (h *InspectorHandler) ListInspectors(c *gin.Context) {
	status := c.Query("status")

	query := `
		SELECT 
			id,
			name,
			inspector_id,
			email,
			certification_level,
			status,
			metadata,
			created_at,
			updated_at
		FROM inspectors
		WHERE deleted_at IS NULL
	`
	args := []interface{}{}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}

	query += " ORDER BY id DESC"

	var dbRows []inspectorRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		logger.Printf("[INSPECTOR] Failed to query inspectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list inspectors"})
		return
	}

	inspectors := []InspectorResponse{}
	for _, i := range dbRows {
		email := ""
		if i.Email.Valid {
			email = i.Email.String
		}
		certLevel := "level_1"
		if i.CertificationLevel != "" {
			certLevel = i.CertificationLevel
		}
		var metadata interface{}
		if i.Metadata.Valid && i.Metadata.String != "" && i.Metadata.String != "null" {
			metadata = parseJSONRaw(i.Metadata.String)
		}
		createdAt := ""
		if i.CreatedAt.Valid {
			createdAt = i.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		updatedAt := ""
		if i.UpdatedAt.Valid {
			updatedAt = i.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}

		inspectors = append(inspectors, InspectorResponse{
			ID:                 fmt.Sprintf("%d", i.ID),
			Name:               i.Name,
			InspectorID:        i.InspectorID,
			Email:              email,
			CertificationLevel: certLevel,
			Status:             i.Status,
			Metadata:           metadata,
			CreatedAt:          createdAt,
			UpdatedAt:          updatedAt,
		})
	}

	c.JSON(http.StatusOK, InspectorListResponse{
		Inspectors: inspectors,
	})
}

// GetInspector handles getting a single inspector by ID.
//
// @Summary      Get inspector
// @Description  Gets an inspector by ID
// @Tags         inspectors
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Inspector ID"
// @Success      200  {object}  InspectorResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /inspectors/{id} [get]
func (h *InspectorHandler) GetInspector(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid inspector id"})
		return
	}

	query := `
		SELECT 
			id,
			name,
			inspector_id,
			email,
			certification_level,
			status,
			metadata,
			created_at,
			updated_at
		FROM inspectors
		WHERE id = ? AND deleted_at IS NULL
	`

	var i inspectorRow
	if err := h.db.Get(&i, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "inspector not found"})
			return
		}
		logger.Printf("[INSPECTOR] Failed to query inspector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get inspector"})
		return
	}

	email := ""
	if i.Email.Valid {
		email = i.Email.String
	}
	certLevel := "level_1"
	if i.CertificationLevel != "" {
		certLevel = i.CertificationLevel
	}
	var metadata interface{}
	if i.Metadata.Valid && i.Metadata.String != "" && i.Metadata.String != "null" {
		metadata = parseJSONRaw(i.Metadata.String)
	}
	createdAt := ""
	if i.CreatedAt.Valid {
		createdAt = i.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if i.UpdatedAt.Valid {
		updatedAt = i.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, InspectorResponse{
		ID:                 fmt.Sprintf("%d", i.ID),
		Name:               i.Name,
		InspectorID:        i.InspectorID,
		Email:              email,
		CertificationLevel: certLevel,
		Status:             i.Status,
		Metadata:           metadata,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	})
}

// CreateInspector handles inspector creation requests.
//
// @Summary      Create inspector
// @Description  Creates a new inspector
// @Tags         inspectors
// @Accept       json
// @Produce      json
// @Param        body  body      CreateInspectorRequest  true  "Inspector payload"
// @Success      201   {object}  CreateInspectorResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /inspectors [post]
func (h *InspectorHandler) CreateInspector(c *gin.Context) {
	var req CreateInspectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.InspectorID = strings.TrimSpace(req.InspectorID)
	req.Email = strings.TrimSpace(req.Email)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.InspectorID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "inspector_id is required"})
		return
	}

	// Check if inspector_id already exists
	var exists bool
	err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM inspectors WHERE inspector_id = ? AND deleted_at IS NULL)", req.InspectorID)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "inspector_id already exists"})
		return
	}

	// Validate certification level
	validCertLevels := map[string]bool{
		"level_1": true,
		"level_2": true,
		"senior":  true,
	}
	certLevel := "level_1"
	if req.CertificationLevel != "" {
		if !validCertLevels[req.CertificationLevel] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid certification_level, must be one of: level_1, level_2, senior"})
			return
		}
		certLevel = req.CertificationLevel
	}

	var emailStr sql.NullString
	if req.Email != "" {
		emailStr = sql.NullString{String: req.Email, Valid: true}
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

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO inspectors (
			name,
			inspector_id,
			email,
			certification_level,
			status,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name,
		req.InspectorID,
		emailStr,
		certLevel,
		"active",
		metadataStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to insert inspector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create inspector"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create inspector"})
		return
	}

	var metaOut interface{}
	if metadataStr.Valid {
		metaOut = parseJSONRaw(metadataStr.String)
	}

	c.JSON(http.StatusCreated, CreateInspectorResponse{
		ID:                 fmt.Sprintf("%d", id),
		Name:               req.Name,
		InspectorID:        req.InspectorID,
		CertificationLevel: certLevel,
		Status:             "active",
		Metadata:           metaOut,
		CreatedAt:          now.Format(time.RFC3339),
	})
}

// UpdateInspector handles updating an inspector.
//
// @Summary      Update inspector
// @Description  Updates an existing inspector
// @Tags         inspectors
// @Accept       json
// @Produce      json
// @Param        id   path      string             true  "Inspector ID"
// @Param        body body      UpdateInspectorRequest  true  "Inspector payload"
// @Success      200  {object}  InspectorResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /inspectors/{id} [put]
func (h *InspectorHandler) UpdateInspector(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid inspector id"})
		return
	}

	var req UpdateInspectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Validate certification level and status if provided
	validCertLevels := map[string]bool{
		"level_1": true,
		"level_2": true,
		"senior":  true,
	}
	validStatuses := map[string]bool{
		"active":   true,
		"inactive": true,
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	if req.InspectorID != nil {
		inspectorID := strings.TrimSpace(*req.InspectorID)
		if inspectorID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "inspector_id cannot be empty"})
			return
		}
		var taken bool
		err = h.db.Get(&taken, "SELECT EXISTS(SELECT 1 FROM inspectors WHERE inspector_id = ? AND id != ? AND deleted_at IS NULL)", inspectorID, id)
		if err != nil {
			logger.Printf("[INSPECTOR] Failed to check inspector_id: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update inspector"})
			return
		}
		if taken {
			c.JSON(http.StatusBadRequest, gin.H{"error": "inspector_id already exists"})
			return
		}
		updates = append(updates, "inspector_id = ?")
		args = append(args, inspectorID)
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

	if req.CertificationLevel != nil {
		certLevel := strings.TrimSpace(*req.CertificationLevel)
		if !validCertLevels[certLevel] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid certification_level, must be one of: level_1, level_2, senior"})
			return
		}
		updates = append(updates, "certification_level = ?")
		args = append(args, certLevel)
	}

	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if !validStatuses[status] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status, must be one of: active, inactive"})
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

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE inspectors SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	result, err := h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to update inspector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update inspector"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "inspector not found"})
		return
	}

	// Fetch the updated inspector
	var i inspectorRow
	err = h.db.Get(&i, "SELECT id, name, inspector_id, email, certification_level, status, metadata, created_at, updated_at FROM inspectors WHERE id = ?", id)
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to fetch updated inspector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated inspector"})
		return
	}

	email := ""
	if i.Email.Valid {
		email = i.Email.String
	}
	certLevel := "level_1"
	if i.CertificationLevel != "" {
		certLevel = i.CertificationLevel
	}
	var metadata interface{}
	if i.Metadata.Valid && i.Metadata.String != "" && i.Metadata.String != "null" {
		metadata = parseJSONRaw(i.Metadata.String)
	}
	createdAt := ""
	if i.CreatedAt.Valid {
		createdAt = i.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if i.UpdatedAt.Valid {
		updatedAt = i.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, InspectorResponse{
		ID:                 fmt.Sprintf("%d", i.ID),
		Name:               i.Name,
		InspectorID:        i.InspectorID,
		Email:              email,
		CertificationLevel: certLevel,
		Status:             i.Status,
		Metadata:           metadata,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	})
}

// DeleteInspector handles inspector deletion requests (soft delete).
//
// @Summary      Delete inspector
// @Description  Soft deletes an inspector by ID
// @Tags         inspectors
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Inspector ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /inspectors/{id} [delete]
func (h *InspectorHandler) DeleteInspector(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid inspector id"})
		return
	}

	// Check if inspector exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM inspectors WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to check inspector existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete inspector"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "inspector not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE inspectors SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[INSPECTOR] Failed to delete inspector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete inspector"})
		return
	}

	c.Status(http.StatusNoContent)
}
