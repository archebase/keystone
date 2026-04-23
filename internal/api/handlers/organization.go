// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"crypto/rand"
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
	FactoryID   string      `json:"factory_id"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
	CreatedAt   string      `json:"created_at,omitempty"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
}

// OrganizationListResponse represents the response for listing organizations.
type OrganizationListResponse struct {
	Items   []OrganizationResponse `json:"items"`
	Total   int                    `json:"total"`
	Limit   int                    `json:"limit"`
	Offset  int                    `json:"offset"`
	HasNext bool                   `json:"hasNext,omitempty"`
	HasPrev bool                   `json:"hasPrev,omitempty"`
}

// CreateOrganizationRequest represents the request body for creating an organization.
// The server assigns a unique slug (org- plus 10 random [a-z0-9] characters).
type CreateOrganizationRequest struct {
	FactoryID   string      `json:"factory_id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Settings    interface{} `json:"settings,omitempty"`
}

// CreateOrganizationResponse represents the response for creating an organization.
type CreateOrganizationResponse struct {
	ID          string `json:"id"`
	FactoryID   string `json:"factory_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// organizationSettingsPatch distinguishes a missing "settings" key from JSON null.
// present: key appeared in the body; isNull: value was JSON null (stored as {}).
type organizationSettingsPatch struct {
	present bool
	isNull  bool
	raw     json.RawMessage
}

func (p *organizationSettingsPatch) UnmarshalJSON(data []byte) error {
	p.present = true
	if string(data) == "null" {
		p.isNull = true
		return nil
	}
	p.raw = append(json.RawMessage(nil), data...)
	return nil
}

// UpdateOrganizationRequest represents the request body for updating an organization.
// Organization slug is immutable after creation.
type UpdateOrganizationRequest struct {
	Name        string                    `json:"name,omitempty"`
	Description *string                   `json:"description,omitempty"`
	Settings    organizationSettingsPatch `json:"settings,omitempty"`
}

// RegisterRoutes registers organization related routes.
func (h *OrganizationHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/organizations", h.ListOrganizations)
	apiV1.POST("/organizations", h.CreateOrganization)
	apiV1.GET("/organizations/:id", h.GetOrganization)
	apiV1.PUT("/organizations/:id", h.UpdateOrganization)
	apiV1.DELETE("/organizations/:id", h.DeleteOrganization)
}

const (
	orgSlugPrefix   = "org-"
	orgSlugRandLen  = 10
	orgSlugRetries  = 20
	orgSlugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func randomOrgSlugSuffix() (string, error) {
	raw := make([]byte, orgSlugRandLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, orgSlugRandLen)
	for i := range out {
		out[i] = orgSlugAlphabet[int(raw[i])%len(orgSlugAlphabet)]
	}
	return string(out), nil
}

func (h *OrganizationHandler) allocateOrganizationSlug() (string, error) {
	for i := 0; i < orgSlugRetries; i++ {
		suffix, err := randomOrgSlugSuffix()
		if err != nil {
			return "", err
		}
		slug := orgSlugPrefix + suffix
		var exists bool
		if err := h.db.Get(&exists,
			"SELECT EXISTS(SELECT 1 FROM organizations WHERE slug = ? AND deleted_at IS NULL)", slug); err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
	}
	return "", fmt.Errorf("could not allocate unique organization slug")
}

// organizationRow represents an organization in the database
type organizationRow struct {
	ID          int64          `db:"id"`
	FactoryID   int64          `db:"factory_id"`
	Name        string         `db:"name"`
	Slug        string         `db:"slug"`
	Description sql.NullString `db:"description"`
	Settings    sql.NullString `db:"settings"`
	CreatedAt   sql.NullTime   `db:"created_at"`
	UpdatedAt   sql.NullTime   `db:"updated_at"`
}

// ListOrganizations handles organization listing requests.
//
// @Summary      List organizations
// @Description  Lists all organizations with pagination
// @Tags         organizations
// @Accept       json
// @Produce      json
// @Param        limit  query     int  false  "Max results (default 50, max 100)"
// @Param        offset query     int  false  "Pagination offset (default 0)"
// @Success      200 {object} OrganizationListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /organizations [get]
func (h *OrganizationHandler) ListOrganizations(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	countQuery := "SELECT COUNT(*) FROM organizations WHERE deleted_at IS NULL"
	var total int
	if err := h.db.Get(&total, countQuery); err != nil {
		logger.Printf("[ORGANIZATION] Failed to count organizations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
		return
	}

	query := `
		SELECT 
			o.id,
			o.factory_id,
			o.name,
			o.slug,
			o.description,
			o.settings,
			o.created_at,
			o.updated_at
		FROM organizations o
		WHERE o.deleted_at IS NULL
		ORDER BY o.id DESC
		LIMIT ? OFFSET ?
	`

	var dbRows []organizationRow
	if err := h.db.Select(&dbRows, query, pagination.Limit, pagination.Offset); err != nil {
		logger.Printf("[ORGANIZATION] Failed to query organizations: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list organizations"})
		return
	}

	organizations := []OrganizationResponse{}
	for _, org := range dbRows {
		organizations = append(organizations, orgResponseFromRow(org))
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, OrganizationListResponse{
		Items:   organizations,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
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

	var org organizationRow
	if err := h.db.Get(&org, `
		SELECT id, factory_id, name, slug, description, settings, created_at, updated_at
		FROM organizations
		WHERE id = ? AND deleted_at IS NULL
	`, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "organization not found"})
			return
		}
		logger.Printf("[ORGANIZATION] Failed to query organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get organization"})
		return
	}

	c.JSON(http.StatusOK, orgResponseFromRow(org))
}

// CreateOrganization handles organization creation requests.
//
// @Summary      Create organization
// @Description  Creates a new organization. The server assigns a unique org- prefix plus 10 random characters as slug. Organization names must be unique among non-deleted rows.
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

	req.FactoryID = strings.TrimSpace(req.FactoryID)
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)

	if req.FactoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id is required"})
		return
	}

	factoryID, err := strconv.ParseInt(req.FactoryID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
		return
	}

	// Verify factory exists
	var factoryExists bool
	if err := h.db.Get(&factoryExists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID); err != nil || !factoryExists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	var nameTaken bool
	if err := h.db.Get(&nameTaken,
		"SELECT EXISTS(SELECT 1 FROM organizations WHERE name = ? AND deleted_at IS NULL)", req.Name); err != nil {
		logger.Printf("[ORGANIZATION] Failed to check organization name: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
		return
	}
	if nameTaken {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization name already exists"})
		return
	}

	slug, err := h.allocateOrganizationSlug()
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to allocate slug: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create organization"})
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
			factory_id,
			name,
			slug,
			description,
			settings,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		factoryID,
		req.Name,
		slug,
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
		FactoryID:   req.FactoryID,
		Name:        req.Name,
		Slug:        slug,
		Description: req.Description,
		CreatedAt:   now.Format(time.RFC3339),
	})
}

// UpdateOrganization handles organization update requests.
//
// @Summary      Update organization
// @Description  Updates an existing organization. Slug cannot be changed. Organization names must be unique among non-deleted rows.
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
		"SELECT id, factory_id, name, slug, description, settings, created_at, updated_at FROM organizations WHERE id = ? AND deleted_at IS NULL",
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

	if req.Name != "" {
		var nameTaken bool
		if err := h.db.Get(&nameTaken,
			"SELECT EXISTS(SELECT 1 FROM organizations WHERE name = ? AND id != ? AND deleted_at IS NULL)", req.Name, id); err != nil {
			logger.Printf("[ORGANIZATION] Failed to check organization name: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
			return
		}
		if nameTaken {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization name already exists"})
			return
		}
		updates = append(updates, "name = ?")
		args = append(args, req.Name)
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

	if req.Settings.present {
		var raw json.RawMessage
		if req.Settings.isNull {
			raw = nil
		} else {
			raw = req.Settings.raw
		}
		updates = append(updates, "settings = ?")
		args = append(args, sql.NullString{String: jsonStringOrEmptyObject(raw), Valid: true})
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE organizations SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to update organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update organization"})
		return
	}

	// Fetch the updated organization
	var org organizationRow
	err = h.db.Get(&org,
		"SELECT id, factory_id, name, slug, description, settings, created_at, updated_at FROM organizations WHERE id = ?",
		id)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to fetch updated organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated organization"})
		return
	}

	c.JSON(http.StatusOK, orgResponseFromRow(org))
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

	// Block deletion if the organization still has active data_collectors, inspectors, or orders.
	var dcCount int
	if err = h.db.Get(&dcCount, "SELECT COUNT(*) FROM data_collectors WHERE organization_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORGANIZATION] Failed to check data_collector count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}
	if dcCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete organization with %d associated data collectors", dcCount)})
		return
	}

	var inspectorCount int
	if err = h.db.Get(&inspectorCount, "SELECT COUNT(*) FROM inspectors WHERE organization_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORGANIZATION] Failed to check inspector count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}
	if inspectorCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete organization with %d associated inspectors", inspectorCount)})
		return
	}

	var orderCount int
	if err = h.db.Get(&orderCount, "SELECT COUNT(*) FROM orders WHERE organization_id = ? AND deleted_at IS NULL", id); err != nil {
		logger.Printf("[ORGANIZATION] Failed to check order count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}
	if orderCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete organization with %d associated orders", orderCount)})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE organizations SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[ORGANIZATION] Failed to delete organization: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete organization"})
		return
	}

	c.Status(http.StatusNoContent)
}

// orgResponseFromRow converts an organizationRow to an OrganizationResponse.
func orgResponseFromRow(org organizationRow) OrganizationResponse {
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
		createdAt = org.CreatedAt.Time.UTC().Format(time.RFC3339)
	}

	updatedAt := ""
	if org.UpdatedAt.Valid {
		updatedAt = org.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	return OrganizationResponse{
		ID:          fmt.Sprintf("%d", org.ID),
		FactoryID:   fmt.Sprintf("%d", org.FactoryID),
		Name:        org.Name,
		Slug:        org.Slug,
		Description: description,
		Settings:    settings,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}
}
