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
	Name          string   `json:"name"`
	Slug          *string  `json:"slug,omitempty"`
	Description   string   `json:"description,omitempty"`
	SkillSequence []string `json:"skill_sequence"`
	Version       int      `json:"version"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

// SOPListResponse represents the response for listing SOPs.
type SOPListResponse struct {
	SOPs []SOPResponse `json:"sops"`
}

// CreateSOPRequest represents the request body for creating an SOP.
type CreateSOPRequest struct {
	Name          string   `json:"name"`
	Slug          string   `json:"slug"`
	Description   string   `json:"description,omitempty"`
	SkillSequence []string `json:"skill_sequence"`
	Version       *int     `json:"version,omitempty"`
}

// CreateSOPResponse represents the response for creating an SOP.
type CreateSOPResponse struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Slug          *string  `json:"slug,omitempty"`
	SkillSequence []string `json:"skill_sequence"`
	Version       int      `json:"version"`
	CreatedAt     string   `json:"created_at"`
}

// UpdateSOPRequest represents the request body for updating an SOP.
type UpdateSOPRequest struct {
	Name          *string   `json:"name,omitempty"`
	Slug          *string   `json:"slug,omitempty"`
	Description   *string   `json:"description,omitempty"`
	SkillSequence *[]string `json:"skill_sequence,omitempty"`
	Version       *int      `json:"version,omitempty"`
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
	Name          string         `db:"name"`
	Slug          sql.NullString `db:"slug"`
	Description   sql.NullString `db:"description"`
	SkillSequence string         `db:"skill_sequence"`
	Version       int            `db:"version"`
	CreatedAt     sql.NullString `db:"created_at"`
	UpdatedAt     sql.NullString `db:"updated_at"`
}

// ListSOPs handles SOP listing requests.
//
// @Summary      List SOPs
// @Description  Lists all SOPs
// @Tags         sops
// @Accept       json
// @Produce      json
// @Success      200 {object} SOPListResponse
// @Failure      500 {object} map[string]string
// @Router       /sops [get]
func (h *SOPHandler) ListSOPs(c *gin.Context) {
	query := `
		SELECT 
			id,
			name,
			slug,
			description,
			skill_sequence,
			version,
			created_at,
			updated_at
		FROM sops
		WHERE deleted_at IS NULL
		ORDER BY id DESC
	`

	var dbRows []sopRow
	if err := h.db.Select(&dbRows, query); err != nil {
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
		var slug *string
		if s.Slug.Valid && strings.TrimSpace(s.Slug.String) != "" {
			v := s.Slug.String
			slug = &v
		}
		skillSequence := parseJSONArray(s.SkillSequence)
		createdAt := ""
		if s.CreatedAt.Valid {
			createdAt = s.CreatedAt.String
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.String
		}

		sops = append(sops, SOPResponse{
			ID:            fmt.Sprintf("%d", s.ID),
			Name:          s.Name,
			Slug:          slug,
			Description:   description,
			SkillSequence: skillSequence,
			Version:       s.Version,
			CreatedAt:     createdAt,
			UpdatedAt:     updatedAt,
		})
	}

	c.JSON(http.StatusOK, SOPListResponse{
		SOPs: sops,
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
			name,
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
	var slug *string
	if s.Slug.Valid && strings.TrimSpace(s.Slug.String) != "" {
		v := s.Slug.String
		slug = &v
	}
	skillSequence := parseJSONArray(s.SkillSequence)
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SOPResponse{
		ID:            fmt.Sprintf("%d", s.ID),
		Name:          s.Name,
		Slug:          slug,
		Description:   description,
		SkillSequence: skillSequence,
		Version:       s.Version,
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

	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Description = strings.TrimSpace(req.Description)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	var slugStr sql.NullString
	var slugResp *string
	if req.Slug != "" {
		// Validate slug format
		if !isValidSlug(req.Slug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug must contain only alphanumeric characters and hyphens"})
			return
		}

		// Check if slug already exists
		var exists bool
		err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM sops WHERE slug = ? AND deleted_at IS NULL)", req.Slug)
		if err == nil && exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists"})
			return
		}

		slugStr = sql.NullString{String: req.Slug, Valid: true}
		v := req.Slug
		slugResp = &v
	}

	// Validate skill_sequence
	if len(req.SkillSequence) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill_sequence is required and must not be empty"})
		return
	}

	version := 1
	if req.Version != nil {
		if *req.Version < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version must be >= 1"})
			return
		}
		version = *req.Version
	}

	// Convert skill_sequence to JSON string
	skillSeqJSON, err := json.Marshal(req.SkillSequence)
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
			name,
			slug,
			description,
			skill_sequence,
			version,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.Name,
		slugStr,
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
		Name:          req.Name,
		Slug:          slugResp,
		SkillSequence: req.SkillSequence,
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
		if slug == "" {
			updates = append(updates, "slug = ?")
			args = append(args, sql.NullString{Valid: false})
		} else {
			if !isValidSlug(slug) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug must contain only alphanumeric characters and hyphens"})
				return
			}
			// Check if slug already exists for another SOP
			var exists bool
			err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM sops WHERE slug = ? AND id != ? AND deleted_at IS NULL)", slug, id)
			if err == nil && exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists"})
				return
			}
			updates = append(updates, "slug = ?")
			args = append(args, slug)
		}
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

	if req.Version != nil {
		if *req.Version < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "version must be >= 1"})
			return
		}
		updates = append(updates, "version = ?")
		args = append(args, *req.Version)
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE sops SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	result, err := h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[SOP] Failed to update SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update SOP"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "SOP not found"})
		return
	}

	// Fetch the updated SOP
	var s sopRow
	err = h.db.Get(&s, "SELECT id, name, slug, description, skill_sequence, version, created_at, updated_at FROM sops WHERE id = ?", id)
	if err != nil {
		logger.Printf("[SOP] Failed to fetch updated SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated SOP"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	var slug *string
	if s.Slug.Valid && strings.TrimSpace(s.Slug.String) != "" {
		v := s.Slug.String
		slug = &v
	}
	skillSequence := parseJSONArray(s.SkillSequence)
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SOPResponse{
		ID:            fmt.Sprintf("%d", s.ID),
		Name:          s.Name,
		Slug:          slug,
		Description:   description,
		SkillSequence: skillSequence,
		Version:       s.Version,
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
	_, err = h.db.Exec("UPDATE sops SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[SOP] Failed to delete SOP: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete SOP"})
		return
	}

	c.Status(http.StatusNoContent)
}
