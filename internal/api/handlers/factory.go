// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
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

// FactoryHandler handles factory related HTTP requests.
type FactoryHandler struct {
	db *sqlx.DB
}

// NewFactoryHandler creates a new FactoryHandler.
func NewFactoryHandler(db *sqlx.DB) *FactoryHandler {
	return &FactoryHandler{db: db}
}

// FactoryResponse represents a factory in the response.
type FactoryResponse struct {
	ID             string      `json:"id"`
	OrganizationID string      `json:"organization_id"`
	Name           string      `json:"name"`
	Slug           string      `json:"slug"`
	Location       string      `json:"location,omitempty"`
	Timezone       string      `json:"timezone,omitempty"`
	Settings       interface{} `json:"settings,omitempty"`
	CreatedAt      string      `json:"created_at,omitempty"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
}

// FactoryListResponse represents the response for listing factories.
type FactoryListResponse struct {
	Factories []FactoryResponse `json:"factories"`
}

// CreateFactoryRequest represents the request body for creating a factory.
type CreateFactoryRequest struct {
	OrganizationID string      `json:"organization_id"`
	Name           string      `json:"name"`
	Slug           string      `json:"slug"`
	Location       string      `json:"location,omitempty"`
	Timezone       string      `json:"timezone,omitempty"`
	Settings       interface{} `json:"settings,omitempty"`
}

// CreateFactoryResponse represents the response for creating a factory.
type CreateFactoryResponse struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	Location       string `json:"location,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
	CreatedAt      string `json:"created_at"`
}

// RegisterRoutes registers factory related routes.
func (h *FactoryHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/factories", h.ListFactories)
	apiV1.POST("/factories", h.CreateFactory)
	apiV1.GET("/factories/:id", h.GetFactory)
	apiV1.PUT("/factories/:id", h.ReplaceFactory)
	apiV1.PATCH("/factories/:id", h.UpdateFactory)
	apiV1.DELETE("/factories/:id", h.DeleteFactory)
}

// factoryRow represents a factory in the database
type factoryRow struct {
	ID             int64          `db:"id"`
	OrganizationID int64          `db:"organization_id"`
	Name           string         `db:"name"`
	Slug           string         `db:"slug"`
	Location       sql.NullString `db:"location"`
	Timezone       sql.NullString `db:"timezone"`
	Settings       sql.NullString `db:"settings"`
	CreatedAt      sql.NullString `db:"created_at"`
	UpdatedAt      sql.NullString `db:"updated_at"`
}

// ListFactories handles factory listing requests with filtering.
//
// @Summary      List factories
// @Description  Lists factories with optional filtering by organization_id
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        organization_id query     string  false  "Filter by organization ID"
// @Success      200               {object}  FactoryListResponse
// @Failure      500               {object}  map[string]string
// @Router       /factories [get]
func (h *FactoryHandler) ListFactories(c *gin.Context) {
	orgID := c.Query("organization_id")

	query := `
		SELECT 
			id,
			organization_id,
			name,
			slug,
			location,
			timezone,
			settings,
			created_at,
			updated_at
		FROM factories
		WHERE deleted_at IS NULL
	`
	args := []interface{}{}

	if orgID != "" {
		parsedOrgID, err := strconv.ParseInt(orgID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
			return
		}
		query += " AND organization_id = ?"
		args = append(args, parsedOrgID)
	}

	query += " ORDER BY id DESC"

	factories := []FactoryResponse{}

	// Use db.Select for cleaner code and automatic resource management
	var dbRows []factoryRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		logger.Printf("[FACTORY] Failed to query factories: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list factories"})
		return
	}

	for _, f := range dbRows {
		location := ""
		if f.Location.Valid {
			location = f.Location.String
		}
		timezone := "UTC"
		if f.Timezone.Valid {
			timezone = f.Timezone.String
		}
		createdAt := ""
		if f.CreatedAt.Valid {
			createdAt = f.CreatedAt.String
		}

		factories = append(factories, FactoryResponse{
			ID:             fmt.Sprintf("%d", f.ID),
			OrganizationID: fmt.Sprintf("%d", f.OrganizationID),
			Name:           f.Name,
			Slug:           f.Slug,
			Location:       location,
			Timezone:       timezone,
			CreatedAt:      createdAt,
		})
	}

	c.JSON(http.StatusOK, FactoryListResponse{
		Factories: factories,
	})
}

// CreateFactory handles factory creation requests.
//
// @Summary      Create factory
// @Description  Creates a new factory
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        body  body      CreateFactoryRequest  true  "Factory payload"
// @Success      201   {object}  CreateFactoryResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /factories [post]
func (h *FactoryHandler) CreateFactory(c *gin.Context) {
	var req CreateFactoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.OrganizationID = strings.TrimSpace(req.OrganizationID)
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Location = strings.TrimSpace(req.Location)
	req.Timezone = strings.TrimSpace(req.Timezone)

	if req.OrganizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id is required"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.Slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug is required"})
		return
	}

	// Parse organization_id
	orgID, err := strconv.ParseInt(req.OrganizationID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
		return
	}

	// Verify organization exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id = ? AND deleted_at IS NULL)", orgID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization not found"})
		return
	}

	// Check if slug already exists for this organization
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE organization_id = ? AND slug = ? AND deleted_at IS NULL)", orgID, req.Slug)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this organization"})
		return
	}

	now := time.Now().UTC()

	// Set default timezone if not provided
	timezone := req.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	// Convert location to nullable string
	var locationStr sql.NullString
	if req.Location != "" {
		locationStr = sql.NullString{String: req.Location, Valid: true}
	}

	// Convert timezone to nullable string
	var timezoneStr sql.NullString
	timezoneStr = sql.NullString{String: timezone, Valid: true}

	// Convert settings to JSON string if provided
	var settingsStr sql.NullString
	if req.Settings != nil {
		settingsJSON, err := json.Marshal(req.Settings)
		if err == nil {
			settingsStr = sql.NullString{String: string(settingsJSON), Valid: true}
		}
	}

	result, err := h.db.Exec(
		`INSERT INTO factories (
			organization_id,
			name,
			slug,
			location,
			timezone,
			settings,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID,
		req.Name,
		req.Slug,
		locationStr,
		timezoneStr,
		settingsStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[FACTORY] Failed to insert factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[FACTORY] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
		return
	}

	c.JSON(http.StatusCreated, CreateFactoryResponse{
		ID:             fmt.Sprintf("%d", id),
		OrganizationID: req.OrganizationID,
		Name:           req.Name,
		Slug:           req.Slug,
		Location:       req.Location,
		Timezone:       timezone,
		CreatedAt:      now.Format(time.RFC3339),
	})
}

// GetFactory handles getting a single factory by ID.
//
// @Summary      Get factory
// @Description  Gets a factory by ID
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Factory ID"
// @Success      200  {object}  FactoryResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /factories/{id} [get]
func (h *FactoryHandler) GetFactory(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory id"})
		return
	}

	query := `
		SELECT 
			id,
			organization_id,
			name,
			slug,
			location,
			timezone,
			settings,
			created_at,
			updated_at
		FROM factories
		WHERE id = ? AND deleted_at IS NULL
	`

	var f factoryRow
	if err := h.db.Get(&f, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
			return
		}
		logger.Printf("[FACTORY] Failed to query factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get factory"})
		return
	}

	location := ""
	if f.Location.Valid {
		location = f.Location.String
	}
	timezone := "UTC"
	if f.Timezone.Valid {
		timezone = f.Timezone.String
	}
	createdAt := ""
	if f.CreatedAt.Valid {
		createdAt = f.CreatedAt.String
	}
	updatedAt := ""
	if f.UpdatedAt.Valid {
		updatedAt = f.UpdatedAt.String
	}

	c.JSON(http.StatusOK, FactoryResponse{
		ID:             fmt.Sprintf("%d", f.ID),
		OrganizationID: fmt.Sprintf("%d", f.OrganizationID),
		Name:           f.Name,
		Slug:           f.Slug,
		Location:       location,
		Timezone:       timezone,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	})
}

// UpdateFactoryRequest represents the request body for updating a factory.
type UpdateFactoryRequest struct {
	Name     *string `json:"name,omitempty"`
	Slug     *string `json:"slug,omitempty"`
	Location *string `json:"location,omitempty"`
	Timezone *string `json:"timezone,omitempty"`
}

// ReplaceFactoryRequest represents the request body for replacing a factory (PUT).
type ReplaceFactoryRequest struct {
	OrganizationID string      `json:"organization_id"`
	Name           string      `json:"name"`
	Slug           string      `json:"slug"`
	Location       string      `json:"location,omitempty"`
	Timezone       string      `json:"timezone,omitempty"`
	Settings       interface{} `json:"settings,omitempty"`
}

// ReplaceFactory handles replacing a factory (full update).
//
// @Summary      Replace factory
// @Description  Replaces an existing factory with the provided data
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        id   path      string                 true  "Factory ID"
// @Param        body body      ReplaceFactoryRequest  true  "Factory payload"
// @Success      200  {object}  FactoryResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /factories/{id} [put]
func (h *FactoryHandler) ReplaceFactory(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory id"})
		return
	}

	var req ReplaceFactoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.OrganizationID = strings.TrimSpace(req.OrganizationID)
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Location = strings.TrimSpace(req.Location)
	req.Timezone = strings.TrimSpace(req.Timezone)

	if req.OrganizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id is required"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.Slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug is required"})
		return
	}

	// Parse organization_id
	orgID, err := strconv.ParseInt(req.OrganizationID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id format"})
		return
	}

	// Verify organization exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id = ? AND deleted_at IS NULL)", orgID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization not found"})
		return
	}

	// Check if factory exists
	var existing factoryRow
	err = h.db.Get(&existing, "SELECT id, organization_id, name, slug, location, timezone, settings, created_at, updated_at FROM factories WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
			return
		}
		logger.Printf("[FACTORY] Failed to query factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to replace factory"})
		return
	}

	// Check if slug already exists for this organization (excluding current factory)
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE organization_id = ? AND slug = ? AND id != ? AND deleted_at IS NULL)", orgID, req.Slug, id)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this organization"})
		return
	}

	now := time.Now().UTC()

	// Set default timezone if not provided
	timezone := req.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	// Convert location to nullable string
	var locationStr sql.NullString
	if req.Location != "" {
		locationStr = sql.NullString{String: req.Location, Valid: true}
	}

	// Convert timezone to nullable string
	var timezoneStr sql.NullString
	timezoneStr = sql.NullString{String: timezone, Valid: true}

	// Convert settings to JSON string if provided
	var settingsStr sql.NullString
	if req.Settings != nil {
		settingsJSON, err := json.Marshal(req.Settings)
		if err == nil {
			settingsStr = sql.NullString{String: string(settingsJSON), Valid: true}
		}
	}

	// Perform full update
	_, err = h.db.Exec(
		`UPDATE factories SET 
			organization_id = ?,
			name = ?,
			slug = ?,
			location = ?,
			timezone = ?,
			settings = ?,
			updated_at = ?
		WHERE id = ?`,
		orgID,
		req.Name,
		req.Slug,
		locationStr,
		timezoneStr,
		settingsStr,
		now,
		id,
	)
	if err != nil {
		logger.Printf("[FACTORY] Failed to replace factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to replace factory"})
		return
	}

	// Fetch the updated factory
	var f factoryRow
	err = h.db.Get(&f, "SELECT id, organization_id, name, slug, location, timezone, settings, created_at, updated_at FROM factories WHERE id = ?", id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to fetch updated factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated factory"})
		return
	}

	location := ""
	if f.Location.Valid {
		location = f.Location.String
	}
	factoryTimezone := "UTC"
	if f.Timezone.Valid {
		factoryTimezone = f.Timezone.String
	}
	createdAt := ""
	if f.CreatedAt.Valid {
		createdAt = f.CreatedAt.String
	}
	updatedAt := ""
	if f.UpdatedAt.Valid {
		updatedAt = f.UpdatedAt.String
	}

	c.JSON(http.StatusOK, FactoryResponse{
		ID:             fmt.Sprintf("%d", f.ID),
		OrganizationID: fmt.Sprintf("%d", f.OrganizationID),
		Name:           f.Name,
		Slug:           f.Slug,
		Location:       location,
		Timezone:       factoryTimezone,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	})
}

// UpdateFactory handles updating a factory.
//
// @Summary      Update factory
// @Description  Updates an existing factory
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        id   path      string                true  "Factory ID"
// @Param        body body      UpdateFactoryRequest  true  "Factory payload"
// @Success      200  {object}  FactoryResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /factories/{id} [patch]
func (h *FactoryHandler) UpdateFactory(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory id"})
		return
	}

	var req UpdateFactoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check if factory exists
	var existing factoryRow
	err = h.db.Get(&existing, "SELECT id, organization_id, name, slug, location, timezone, settings, created_at, updated_at FROM factories WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
			return
		}
		logger.Printf("[FACTORY] Failed to query factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update factory"})
		return
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name != "" {
			updates = append(updates, "name = ?")
			args = append(args, name)
		}
	}

	if req.Slug != nil {
		slug := strings.TrimSpace(*req.Slug)
		if slug != "" {
			// Check if slug already exists for this organization
			var exists bool
			err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE organization_id = ? AND slug = ? AND id != ? AND deleted_at IS NULL)", existing.OrganizationID, slug, id)
			if err == nil && exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this organization"})
				return
			}
			updates = append(updates, "slug = ?")
			args = append(args, slug)
		}
	}

	if req.Location != nil {
		location := strings.TrimSpace(*req.Location)
		var locStr sql.NullString
		if location != "" {
			locStr = sql.NullString{String: location, Valid: true}
		}
		updates = append(updates, "location = ?")
		args = append(args, locStr)
	}

	if req.Timezone != nil {
		tz := strings.TrimSpace(*req.Timezone)
		var tzStr sql.NullString
		if tz != "" {
			tzStr = sql.NullString{String: tz, Valid: true}
		}
		updates = append(updates, "timezone = ?")
		args = append(args, tzStr)
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE factories SET %s WHERE id = ?", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[FACTORY] Failed to update factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update factory"})
		return
	}

	// Fetch the updated factory
	var f factoryRow
	err = h.db.Get(&f, "SELECT id, organization_id, name, slug, location, timezone, settings, created_at, updated_at FROM factories WHERE id = ?", id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to fetch updated factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated factory"})
		return
	}

	location := ""
	if f.Location.Valid {
		location = f.Location.String
	}
	timezone := "UTC"
	if f.Timezone.Valid {
		timezone = f.Timezone.String
	}
	createdAt := ""
	if f.CreatedAt.Valid {
		createdAt = f.CreatedAt.String
	}
	updatedAt := ""
	if f.UpdatedAt.Valid {
		updatedAt = f.UpdatedAt.String
	}

	c.JSON(http.StatusOK, FactoryResponse{
		ID:             fmt.Sprintf("%d", f.ID),
		OrganizationID: fmt.Sprintf("%d", f.OrganizationID),
		Name:           f.Name,
		Slug:           f.Slug,
		Location:       location,
		Timezone:       timezone,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	})
}

// DeleteFactory handles factory deletion requests (soft delete).
//
// @Summary      Delete factory
// @Description  Soft deletes a factory by ID
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Factory ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /factories/{id} [delete]
func (h *FactoryHandler) DeleteFactory(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory id"})
		return
	}

	// Check if factory exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to check factory existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete factory"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE factories SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to delete factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete factory"})
		return
	}

	c.Status(http.StatusNoContent)
}
