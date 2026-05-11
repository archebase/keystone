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

// SOPHandler handles SOP (Standard Operating Procedure) related HTTP requests.
type SOPHandler struct {
	db *sqlx.DB
}

// NewSOPHandler creates a new SOPHandler.
func NewSOPHandler(db *sqlx.DB) *SOPHandler {
	return &SOPHandler{db: db}
}

// SOPResponse represents an SOP in the response.
type SOPResponse struct {
	ID            string   `json:"id"`
	Slug          string   `json:"slug"`
	Description   string   `json:"description,omitempty"`
	SkillSequence []string `json:"skill_sequence"`
	Version       string   `json:"version,omitempty"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

// SOPListResponse represents the response for listing SOPs.
type SOPListResponse struct {
	Items   []SOPResponse `json:"items"`
	Total   int           `json:"total"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
	HasNext bool          `json:"hasNext,omitempty"`
	HasPrev bool          `json:"hasPrev,omitempty"`
}

// CreateSOPRequest represents the request body for creating an SOP.
type CreateSOPRequest struct {
	Slug          string   `json:"slug"`
	Description   string   `json:"description,omitempty"`
	SkillSequence []string `json:"skill_sequence"`
	Version       string   `json:"version,omitempty"`
}

// CreateSOPResponse represents the response for creating an SOP.
type CreateSOPResponse struct {
	ID            string   `json:"id"`
	Slug          string   `json:"slug"`
	SkillSequence []string `json:"skill_sequence"`
	Version       string   `json:"version"`
	CreatedAt     string   `json:"created_at"`
}

// UpdateSOPRequest represents the request body for updating an SOP.
type UpdateSOPRequest struct {
	Slug          *string   `json:"slug,omitempty"`
	Description   *string   `json:"description,omitempty"`
	SkillSequence *[]string `json:"skill_sequence,omitempty"`
	Version       *string   `json:"version,omitempty"`
}

// RegisterRoutes registers SOP related routes.
func (h *SOPHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/sops", h.ListSOPs)
	apiV1.POST("/sops", h.CreateSOP)
	apiV1.GET("/sops/:id", h.GetSOP)
	apiV1.PUT("/sops/:id", h.UpdateSOP)
	apiV1.DELETE("/sops/:id", h.DeleteSOP)
}

// sopRow represents an SOP in the database
type sopRow struct {
	ID            int64          `db:"id"`
	Slug          string         `db:"slug"`
	Description   sql.NullString `db:"description"`
	SkillSequence string         `db:"skill_sequence"`
	Version       sql.NullString `db:"version"`
	CreatedAt     sql.NullTime   `db:"created_at"`
	UpdatedAt     sql.NullTime   `db:"updated_at"`
}

// ListSOPs handles SOP listing requests.
//
// @Summary      List SOPs
// @Description  Lists all SOPs with pagination
// @Tags         sops
// @Accept       json
// @Produce      json
// @Param        slug    query string false "Filter by SOP slug(s), comma-separated"
// @Param        sop_slug query string false "Alias of slug"
// @Param        keyword query string false "Search by slug, description, or version"
// @Param        q       query string false "Alias of keyword"
// @Param        search  query string false "Alias of keyword"
// @Param        limit  query int false "Max results (default 50, max 100)"
// @Param        offset query int false "Pagination offset (default 0)"
// @Success      200 {object} SOPListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /sops [get]
func (h *SOPHandler) ListSOPs(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	slugs, err := parseNonEmptyStringList(firstNonEmptyQuery(c, "slug", "sop_slug"), "slug")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")
	whereClause := "WHERE deleted_at IS NULL"
	args := []any{}
	whereClause, args = appendStringInFilter(whereClause, args, "slug", slugs)
	whereClause, args = appendKeywordSearch(whereClause, args, keyword, "slug", "description", "version")

	countQuery := "SELECT COUNT(*) FROM sops " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[SOP] Failed to count SOPs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list SOPs"})
		return
	}

	orderClause, orderArgs := keywordOrderBy(keyword, "updated_at DESC, id DESC", "slug", "description", "version")
	query := `
		SELECT 
			id,
			slug,
			description,
			skill_sequence,
			version,
			created_at,
			updated_at
		FROM sops
		` + whereClause + `
		` + orderClause + `
		LIMIT ? OFFSET ?
	`
	queryArgs := append(args, orderArgs...)
	queryArgs = append(queryArgs, pagination.Limit, pagination.Offset)

	var dbRows []sopRow
	if err := h.db.Select(&dbRows, query, queryArgs...); err != nil {
		logger.Printf("[SOP] Failed to query SOPs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list SOPs"})
		return
	}

	sops := []SOPResponse{}
	for _, s := range dbRows {
		description := ""
		if s.Description.Valid {
			description = s.Description.String
		}
		skillSequence := parseJSONArray(s.SkillSequence)
		version := "1.0.0"
		if s.Version.Valid {
			version = s.Version.String
		}
		createdAt := ""
		if s.CreatedAt.Valid {
			createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}

		sops = append(sops, SOPResponse{
			ID:            fmt.Sprintf("%d", s.ID),
			Slug:          s.Slug,
			Description:   description,
			SkillSequence: skillSequence,
			Version:       version,
			CreatedAt:     createdAt,
			UpdatedAt:     updatedAt,
		})
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, SOPListResponse{
		Items:   sops,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// GetSOP handles getting a single SOP by ID.
//
// @Summary      Get SOP
// @Description  Gets an SOP by ID
// @Tags         sops
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "SOP ID"
// @Success      200  {object}  SOPResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /sops/{id} [get]
func (h *SOPHandler) GetSOP(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid SOP id"})
		return
	}

	query := `
		SELECT 
			id,
			slug,
			description,
			skill_sequence,
			version,
			created_at,
			updated_at
		FROM sops
		WHERE id = ? AND deleted_at IS NULL
	`

	var s sopRow
	if err := h.db.Get(&s, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "SOP not found"})
			return
		}
		logger.Printf("[SOP] Failed to query SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get SOP"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	skillSequence := parseJSONArray(s.SkillSequence)
	version := "1.0.0"
	if s.Version.Valid {
		version = s.Version.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, SOPResponse{
		ID:            fmt.Sprintf("%d", s.ID),
		Slug:          s.Slug,
		Description:   description,
		SkillSequence: skillSequence,
		Version:       version,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	})
}

// CreateSOP handles SOP creation requests.
//
// @Summary      Create SOP
// @Description  Creates a new SOP
// @Tags         sops
// @Accept       json
// @Produce      json
// @Param        body  body      CreateSOPRequest  true  "SOP payload"
// @Success      201   {object}  CreateSOPResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /sops [post]
func (h *SOPHandler) CreateSOP(c *gin.Context) {
	var req CreateSOPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.Slug = strings.TrimSpace(req.Slug)
	req.Description = strings.TrimSpace(req.Description)
	req.Version = strings.TrimSpace(req.Version)

	if req.Slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug is required"})
		return
	}
	// Validate slug format
	if !isValidSlug(req.Slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": invalidSlugUserMessage})
		return
	}
	version := "1.0.0"
	if req.Version != "" {
		version = req.Version
	}
	// Check if slug already exists for the same version
	var exists bool
	err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM sops WHERE slug = ? AND version = ? AND deleted_at IS NULL)", req.Slug, version)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this version"})
		return
	}

	skillSequence := req.SkillSequence
	if skillSequence == nil {
		skillSequence = []string{}
	}

	// Convert skill_sequence to JSON string
	skillSeqJSON, err := json.Marshal(skillSequence)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process skill_sequence"})
		return
	}

	var descriptionStr sql.NullString
	if req.Description != "" {
		descriptionStr = sql.NullString{String: req.Description, Valid: true}
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO sops (
			slug,
			description,
			skill_sequence,
			version,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		req.Slug,
		descriptionStr,
		string(skillSeqJSON),
		version,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[SOP] Failed to insert SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create SOP"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[SOP] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create SOP"})
		return
	}

	c.JSON(http.StatusCreated, CreateSOPResponse{
		ID:            fmt.Sprintf("%d", id),
		Slug:          req.Slug,
		SkillSequence: skillSequence,
		Version:       version,
		CreatedAt:     now.Format(time.RFC3339),
	})
}

// UpdateSOP handles updating an SOP.
//
// @Summary      Update SOP
// @Description  Updates an existing SOP
// @Tags         sops
// @Accept       json
// @Produce      json
// @Param        id   path      string        true  "SOP ID"
// @Param        body body      UpdateSOPRequest  true  "SOP payload"
// @Success      200  {object}  SOPResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /sops/{id} [put]
func (h *SOPHandler) UpdateSOP(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid SOP id"})
		return
	}

	var req UpdateSOPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Load current SOP to support immutable/version-dependent checks.
	var current struct {
		Slug    string         `db:"slug"`
		Version sql.NullString `db:"version"`
	}
	err = h.db.Get(&current, "SELECT slug, version FROM sops WHERE id = ? AND deleted_at IS NULL", id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "SOP not found"})
		return
	}
	if err != nil {
		logger.Printf("[SOP] Failed to query current SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
		return
	}

	// Build update query dynamically (id-scoped updates only; slug rename is handled separately).
	updates := []string{}
	args := []interface{}{}

	renameSlug := false
	oldSlug := current.Slug
	newSlug := ""
	if req.Slug != nil {
		newSlug = strings.TrimSpace(*req.Slug)
		if newSlug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug cannot be empty"})
			return
		}
		if !isValidSlug(newSlug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": invalidSlugUserMessage})
			return
		}
		renameSlug = newSlug != oldSlug
	}

	if req.Description != nil {
		description := strings.TrimSpace(*req.Description)
		var descStr sql.NullString
		if description != "" {
			descStr = sql.NullString{String: description, Valid: true}
		}
		updates = append(updates, "description = ?")
		args = append(args, descStr)
	}

	if req.SkillSequence != nil {
		skillSeqJSON, err := json.Marshal(*req.SkillSequence)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to process skill_sequence"})
			return
		}
		updates = append(updates, "skill_sequence = ?")
		args = append(args, string(skillSeqJSON))
	}

	// version is immutable after creation; allow no-op payloads that resend the same version
	if req.Version != nil {
		inputVersion := strings.TrimSpace(*req.Version)
		currentVersionStr := "1.0.0"
		if current.Version.Valid {
			currentVersionStr = strings.TrimSpace(current.Version.String)
		}
		if inputVersion != currentVersionStr {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version is immutable and cannot be updated"})
			return
		}
	}

	// It's valid to rename a slug even if no id-scoped fields are being updated.
	if !renameSlug && len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	tx, err := h.db.Beginx()
	if err != nil {
		logger.Printf("[SOP] Failed to begin transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
		return
	}
	defer func() { _ = tx.Rollback() }()

	if renameSlug {
		// Do not allow rename if target slug already exists (any version).
		var exists bool
		if err := tx.Get(&exists, "SELECT EXISTS(SELECT 1 FROM sops WHERE slug = ? AND deleted_at IS NULL)", newSlug); err != nil {
			logger.Printf("[SOP] Failed to check target slug existence: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
			return
		}
		if exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target slug already exists"})
			return
		}

		// Rename all versions under the same old slug.
		if _, err := tx.Exec(
			"UPDATE sops SET slug = ?, updated_at = ? WHERE slug = ? AND deleted_at IS NULL",
			newSlug, now, oldSlug,
		); err != nil {
			logger.Printf("[SOP] Failed to rename SOP slug: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
			return
		}
	}

	if len(updates) > 0 {
		updates = append(updates, "updated_at = ?")
		args = append(args, now)
		args = append(args, id)

		query := fmt.Sprintf("UPDATE sops SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))
		result, err := tx.Exec(query, args...)
		if err != nil {
			logger.Printf("[SOP] Failed to update SOP: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			// MySQL may report 0 rows affected when the row matches but values are unchanged.
			// Re-check existence to avoid false 404 responses on no-op updates.
			var stillExists bool
			if err := tx.Get(&stillExists, "SELECT EXISTS(SELECT 1 FROM sops WHERE id = ? AND deleted_at IS NULL)", id); err != nil {
				logger.Printf("[SOP] Failed to re-check SOP existence: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
				return
			}
			if !stillExists {
				c.JSON(http.StatusNotFound, gin.H{"error": "SOP not found"})
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[SOP] Failed to commit transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
		return
	}

	// Fetch the updated SOP
	var s sopRow
	err = h.db.Get(&s, "SELECT id, slug, description, skill_sequence, version, created_at, updated_at FROM sops WHERE id = ?", id)
	if err != nil {
		logger.Printf("[SOP] Failed to fetch updated SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated SOP"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	skillSequence := parseJSONArray(s.SkillSequence)
	version := "1.0.0"
	if s.Version.Valid {
		version = s.Version.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	c.JSON(http.StatusOK, SOPResponse{
		ID:            fmt.Sprintf("%d", s.ID),
		Slug:          s.Slug,
		Description:   description,
		SkillSequence: skillSequence,
		Version:       version,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	})
}

// DeleteSOP handles SOP deletion requests (soft delete).
//
// @Summary      Delete SOP
// @Description  Soft deletes an SOP by ID
// @Tags         sops
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "SOP ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /sops/{id} [delete]
func (h *SOPHandler) DeleteSOP(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid SOP id"})
		return
	}

	// Check if SOP exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM sops WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[SOP] Failed to check SOP existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete SOP"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "SOP not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE sops SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[SOP] Failed to delete SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete SOP"})
		return
	}

	c.Status(http.StatusNoContent)
}
