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

// OrganizationHandler handles organization related HTTP requests.
type OrganizationHandler struct {
	db *sqlx.DB
}

// NewOrganizationHandler creates a new OrganizationHandler.
func NewOrganizationHandler(db *sqlx.DB) *OrganizationHandler {
	return &OrganizationHandler{db: db}
}

// OrganizationResponse represents an organization in the response.
type OrganizationResponse struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
	CreatedAt   string      `json:"created_at,omitempty"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
}

// OrganizationListResponse represents the response for listing organizations.
type OrganizationListResponse struct {
	Organizations []OrganizationResponse `json:"organizations"`
}

// CreateOrganizationRequest represents the request body for creating an organization.
type CreateOrganizationRequest struct {
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
}

// CreateOrganizationResponse represents the response for creating an organization.
type CreateOrganizationResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// UpdateOrganizationRequest represents the request body for updating an organization.
type UpdateOrganizationRequest struct {
	Name        string      `json:"name,omitempty"`
	Slug        string      `json:"slug,omitempty"`
	Description *string     `json:"description,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
}

// RegisterRoutes registers organization related routes.
func (h *OrganizationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/organizations", h.ListOrganizations)
	apiV1.POST("/organizations", h.CreateOrganization)
	apiV1.GET("/organizations/:id", h.GetOrganization)
	apiV1.PATCH("/organizations/:id", h.UpdateOrganization)
	apiV1.DELETE("/organizations/:id", h.DeleteOrganization)
}

// organizationRow represents an organization in the database
type organizationRow struct {
	ID          int64          `db:"id"`
	Name        string         `db:"name"`
	Slug        string         `db:"slug"`
	Description sql.NullString `db:"description"`
	Settings    sql.NullString `db:"settings"`
	CreatedAt   sql.NullString `db:"created_at"`
	UpdatedAt   sql.NullString `db:"updated_at"`
}

// ListOrganizations handles organization listing requests.
//
// @Summary      List organizations
// @Description  Lists all organizations
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Success      200 {object} OrganizationListResponse
// @Failure      500 {object} map[string]string
// @Router       /organizations [get]
func (h *OrganizationHandler) ListOrganizations(c *gin.Context) {
	query := `
		SELECT 
			id,
			name,
			slug,
			description,
			settings,
			created_at,
			updated_at
		FROM organizations
		WHERE deleted_at IS NULL
		ORDER BY id DESC
	`

	var dbRows []organizationRow
	if err := h.db.Select(&dbRows, query); err != nil {
		logger.Printf("[ORGANIZATION] Failed to query organizations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
		return
	}

	organizations := []OrganizationResponse{}
	for _, org := range dbRows {
		description := ""
		if org.Description.Valid {
			description = org.Description.String
		}

		var settings interface{}
		if org.Settings.Valid && org.Settings.String != "" && org.Settings.String != "null" {
			settings = parseJSONRaw(org.Settings.String)
		}

		createdAt := ""
		if org.CreatedAt.Valid {
			createdAt = org.CreatedAt.String
		}

		updatedAt := ""
		if org.UpdatedAt.Valid {
			updatedAt = org.UpdatedAt.String
		}

		organizations = append(organizations, OrganizationResponse{
			ID:          fmt.Sprintf("%d", org.ID),
			Name:        org.Name,
			Slug:        org.Slug,
			Description: description,
			Settings:    settings,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		})
	}

	c.JSON(http.StatusOK, OrganizationListResponse{
		Organizations: organizations,
	})
}

// GetOrganization handles getting a single organization by ID.
//
// @Summary      Get organization
// @Description  Gets an organization by ID
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Organization ID"
// @Success      200 {object} OrganizationResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /organizations/{id} [get]
func (h *OrganizationHandler) GetOrganization(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
		return
	}

	query := `
		SELECT 
			id,
			name,
			slug,
			description,
			settings,
			created_at,
			updated_at
		FROM organizations
		WHERE id = ? AND deleted_at IS NULL
	`

	var org organizationRow
	if err := h.db.Get(&org, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		logger.Printf("[ORGANIZATION] Failed to query organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
		return
	}

	description := ""
	if org.Description.Valid {
		description = org.Description.String
	}

	var settings interface{}
	if org.Settings.Valid && org.Settings.String != "" && org.Settings.String != "null" {
		settings = parseJSONRaw(org.Settings.String)
	}

	createdAt := ""
	if org.CreatedAt.Valid {
		createdAt = org.CreatedAt.String
	}

	updatedAt := ""
	if org.UpdatedAt.Valid {
		updatedAt = org.UpdatedAt.String
	}

	c.JSON(http.StatusOK, OrganizationResponse{
		ID:          fmt.Sprintf("%d", org.ID),
		Name:        org.Name,
		Slug:        org.Slug,
		Description: description,
		Settings:    settings,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})
}

// CreateOrganization handles organization creation requests.
//
// @Summary      Create organization
// @Description  Creates a new organization
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        body  body      CreateOrganizationRequest  true  "Organization payload"
// @Success      201   {object}  CreateOrganizationResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /organizations [post]
func (h *OrganizationHandler) CreateOrganization(c *gin.Context) {
	var req CreateOrganizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Description = strings.TrimSpace(req.Description)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	if req.Slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug is required"})
		return
	}

	// Validate slug format (alphanumeric and hyphens only)
	if !isValidSlug(req.Slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug must contain only alphanumeric characters and hyphens"})
		return
	}

	// Check if slug already exists
	var exists bool
	err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE slug = ? AND deleted_at IS NULL)", req.Slug)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists"})
		return
	}

	// Convert settings to JSON string if provided
	var settingsStr sql.NullString
	if req.Settings != nil {
		settingsJSON, err := json.Marshal(req.Settings)
		if err == nil {
			settingsStr = sql.NullString{String: string(settingsJSON), Valid: true}
		}
	}

	// Convert description to nullable string
	var descriptionStr sql.NullString
	if req.Description != "" {
		descriptionStr = sql.NullString{String: req.Description, Valid: true}
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO organizations (
			name,
			slug,
			description,
			settings,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		req.Name,
		req.Slug,
		descriptionStr,
		settingsStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to insert organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
		return
	}

	c.JSON(http.StatusCreated, CreateOrganizationResponse{
		ID:          fmt.Sprintf("%d", id),
		Name:        req.Name,
		Slug:        req.Slug,
		Description: req.Description,
		CreatedAt:   now.Format(time.RFC3339),
	})
}

// UpdateOrganization handles organization update requests.
//
// @Summary      Update organization
// @Description  Updates an existing organization
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        id   path      string                 true  "Organization ID"
// @Param        body body      UpdateOrganizationRequest  true  "Organization payload"
// @Success      200 {object} OrganizationResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /organizations/{id} [put]
func (h *OrganizationHandler) UpdateOrganization(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
		return
	}

	var req UpdateOrganizationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check if organization exists
	var existingOrg organizationRow
	err = h.db.Get(&existingOrg,
		"SELECT id, name, slug, description, settings, created_at, updated_at FROM organizations WHERE id = ? AND deleted_at IS NULL",
		id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		logger.Printf("[ORGANIZATION] Failed to query organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
		return
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)

	if req.Name != "" {
		updates = append(updates, "name = ?")
		args = append(args, req.Name)
	}

	if req.Slug != "" {
		// Validate slug format
		if !isValidSlug(req.Slug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug must contain only alphanumeric characters and hyphens"})
			return
		}
		// Check if new slug already exists for another organization
		var exists bool
		err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE slug = ? AND id != ? AND deleted_at IS NULL)", req.Slug, id)
		if err == nil && exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists"})
			return
		}
		updates = append(updates, "slug = ?")
		args = append(args, req.Slug)
	}

	if req.Description != nil {
		desc := strings.TrimSpace(*req.Description)
		var descStr sql.NullString
		if desc != "" {
			descStr = sql.NullString{String: desc, Valid: true}
		}
		updates = append(updates, "description = ?")
		args = append(args, descStr)
	}

	if req.Settings != nil {
		settingsJSON, err := json.Marshal(req.Settings)
		if err == nil {
			updates = append(updates, "settings = ?")
			args = append(args, sql.NullString{String: string(settingsJSON), Valid: true})
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

	query := fmt.Sprintf("UPDATE organizations SET %s WHERE id = ?", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to update organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
		return
	}

	// Fetch the updated organization
	var org organizationRow
	err = h.db.Get(&org,
		"SELECT id, name, slug, description, settings, created_at, updated_at FROM organizations WHERE id = ?",
		id)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to fetch updated organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated organization"})
		return
	}

	description := ""
	if org.Description.Valid {
		description = org.Description.String
	}

	var settings interface{}
	if org.Settings.Valid && org.Settings.String != "" && org.Settings.String != "null" {
		settings = parseJSONRaw(org.Settings.String)
	}

	createdAt := ""
	if org.CreatedAt.Valid {
		createdAt = org.CreatedAt.String
	}

	updatedAt := ""
	if org.UpdatedAt.Valid {
		updatedAt = org.UpdatedAt.String
	}

	c.JSON(http.StatusOK, OrganizationResponse{
		ID:          fmt.Sprintf("%d", org.ID),
		Name:        org.Name,
		Slug:        org.Slug,
		Description: description,
		Settings:    settings,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})
}

// DeleteOrganization handles organization deletion requests (soft delete).
//
// @Summary      Delete organization
// @Description  Soft deletes an organization by ID
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Organization ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /organizations/{id} [delete]
func (h *OrganizationHandler) DeleteOrganization(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization id"})
		return
	}

	// Check if organization exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to check organization existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE organizations SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to delete organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}

	c.Status(http.StatusNoContent)
}

// isValidSlug checks if the slug contains only alphanumeric characters and hyphens
func isValidSlug(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// parseJSONRaw parses a JSON string and returns it as a raw interface{}
func parseJSONRaw(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return s
	}
	return result
}
