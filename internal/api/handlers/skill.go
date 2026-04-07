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

// optionalJSONPatch supports PATCH-style updates: omitted JSON key → Present false;
// key present with null or a value → Present true (Value nil means store metadata as "{}").
// encoding/json does not distinguish null from omitted for *interface{}; this type does.
type optionalJSONPatch struct {
	Present bool
	Value   interface{}
}

func (o *optionalJSONPatch) UnmarshalJSON(data []byte) error {
	o.Present = true
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		o.Value = nil
		return nil
	}
	return json.Unmarshal(data, &o.Value)
}

// SkillHandler handles skill related HTTP requests.
type SkillHandler struct {
	db *sqlx.DB
}

// NewSkillHandler creates a new SkillHandler.
func NewSkillHandler(db *sqlx.DB) *SkillHandler {
	return &SkillHandler{db: db}
}

// SkillResponse represents a skill in the response.
type SkillResponse struct {
	ID          string      `json:"id"`
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Version     string      `json:"version,omitempty"`
	Metadata    interface{} `json:"metadata,omitempty"`
	CreatedAt   string      `json:"created_at,omitempty"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
}

// SkillListResponse represents the response for listing skills.
type SkillListResponse struct {
	Skills []SkillResponse `json:"skills"`
}

// CreateSkillRequest represents the request body for creating a skill.
type CreateSkillRequest struct {
	Slug        string      `json:"slug"`
	Description string      `json:"description,omitempty"`
	Version     string      `json:"version,omitempty"`
	Metadata    interface{} `json:"metadata,omitempty"`
}

// CreateSkillResponse represents the response for creating a skill.
type CreateSkillResponse struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Version   string `json:"version"`
	CreatedAt string `json:"created_at"`
}

// UpdateSkillRequest represents the request body for updating a skill.
// Metadata uses optionalJSONPatch so JSON null (clear) is distinct from omitting the key (unchanged).
type UpdateSkillRequest struct {
	Slug        *string           `json:"slug,omitempty"`
	Description *string           `json:"description,omitempty"`
	Version     *string           `json:"version,omitempty"`
	Metadata    optionalJSONPatch `json:"metadata,omitempty"`
}

// RegisterRoutes registers skill related routes.
func (h *SkillHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/skills", h.ListSkills)
	apiV1.POST("/skills", h.CreateSkill)
	apiV1.GET("/skills/:id", h.GetSkill)
	apiV1.PUT("/skills/:id", h.UpdateSkill)
	apiV1.DELETE("/skills/:id", h.DeleteSkill)
}

// skillRow represents a skill in the database
type skillRow struct {
	ID          int64          `db:"id"`
	Slug        string         `db:"slug"`
	Description sql.NullString `db:"description"`
	Version     sql.NullString `db:"version"`
	Metadata    sql.NullString `db:"metadata"`
	CreatedAt   sql.NullString `db:"created_at"`
	UpdatedAt   sql.NullString `db:"updated_at"`
}

// ListSkills handles skill listing requests.
//
// @Summary      List skills
// @Description  Lists all skills
// @Tags         skills
// @Accept       json
// @Produce      json
// @Success      200 {object} SkillListResponse
// @Failure      500 {object} map[string]string
// @Router       /skills [get]
func (h *SkillHandler) ListSkills(c *gin.Context) {
	query := `
		SELECT 
			id,
			slug,
			description,
			version,
			metadata,
			created_at,
			updated_at
		FROM skills
		WHERE deleted_at IS NULL
		ORDER BY id DESC
	`

	var dbRows []skillRow
	if err := h.db.Select(&dbRows, query); err != nil {
		logger.Printf("[SKILL] Failed to query skills: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list skills"})
		return
	}

	skills := []SkillResponse{}
	for _, s := range dbRows {
		description := ""
		if s.Description.Valid {
			description = s.Description.String
		}
		version := "1.0.0"
		if s.Version.Valid {
			version = s.Version.String
		}
		var metadata interface{}
		if s.Metadata.Valid && s.Metadata.String != "" && s.Metadata.String != "null" {
			metadata = parseJSONRaw(s.Metadata.String)
		}
		createdAt := ""
		if s.CreatedAt.Valid {
			createdAt = s.CreatedAt.String
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.String
		}

		skills = append(skills, SkillResponse{
			ID:          fmt.Sprintf("%d", s.ID),
			Slug:        s.Slug,
			Description: description,
			Version:     version,
			Metadata:    metadata,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		})
	}

	c.JSON(http.StatusOK, SkillListResponse{
		Skills: skills,
	})
}

// GetSkill handles getting a single skill by ID.
//
// @Summary      Get skill
// @Description  Gets a skill by ID
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Skill ID"
// @Success      200  {object}  SkillResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /skills/{id} [get]
func (h *SkillHandler) GetSkill(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill id"})
		return
	}

	query := `
		SELECT 
			id,
			slug,
			description,
			version,
			metadata,
			created_at,
			updated_at
		FROM skills
		WHERE id = ? AND deleted_at IS NULL
	`

	var s skillRow
	if err := h.db.Get(&s, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
			return
		}
		logger.Printf("[SKILL] Failed to query skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get skill"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	version := "1.0.0"
	if s.Version.Valid {
		version = s.Version.String
	}
	var metadata interface{}
	if s.Metadata.Valid && s.Metadata.String != "" && s.Metadata.String != "null" {
		metadata = parseJSONRaw(s.Metadata.String)
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SkillResponse{
		ID:          fmt.Sprintf("%d", s.ID),
		Slug:        s.Slug,
		Description: description,
		Version:     version,
		Metadata:    metadata,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})
}

// CreateSkill handles skill creation requests.
//
// @Summary      Create skill
// @Description  Creates a new skill
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        body  body      CreateSkillRequest  true  "Skill payload"
// @Success      201   {object}  CreateSkillResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /skills [post]
func (h *SkillHandler) CreateSkill(c *gin.Context) {
	var req CreateSkillRequest
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
	err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM skills WHERE slug = ? AND version = ? AND deleted_at IS NULL)", req.Slug, version)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this version"})
		return
	}

	var metadataStr sql.NullString
	if req.Metadata != nil {
		metadataJSON, err := json.Marshal(req.Metadata)
		if err == nil {
			metadataStr = sql.NullString{String: string(metadataJSON), Valid: true}
		}
	}

	var descriptionStr sql.NullString
	if req.Description != "" {
		descriptionStr = sql.NullString{String: req.Description, Valid: true}
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO skills (
			slug,
			description,
			version,
			metadata,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		req.Slug,
		descriptionStr,
		version,
		metadataStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[SKILL] Failed to insert skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create skill"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[SKILL] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create skill"})
		return
	}

	c.JSON(http.StatusCreated, CreateSkillResponse{
		ID:        fmt.Sprintf("%d", id),
		Slug:      req.Slug,
		Version:   version,
		CreatedAt: now.Format(time.RFC3339),
	})
}

// UpdateSkill handles updating a skill.
//
// @Summary      Update skill
// @Description  Updates an existing skill
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        id   path      string           true  "Skill ID"
// @Param        body body      UpdateSkillRequest  true  "Skill payload"
// @Success      200  {object}  SkillResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /skills/{id} [put]
func (h *SkillHandler) UpdateSkill(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill id"})
		return
	}

	var req UpdateSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Load current skill to support immutable/version-dependent checks.
	var current struct {
		Slug    string         `db:"slug"`
		Version sql.NullString `db:"version"`
	}
	err = h.db.Get(&current, "SELECT slug, version FROM skills WHERE id = ? AND deleted_at IS NULL", id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
		return
	}
	if err != nil {
		logger.Printf("[SKILL] Failed to query current skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update skill"})
		return
	}

	// Build update query dynamically
	updates := []string{}
	args := []interface{}{}

	if req.Slug != nil {
		slug := strings.TrimSpace(*req.Slug)
		if slug == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug cannot be empty"})
			return
		}
		if !isValidSlug(slug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": invalidSlugUserMessage})
			return
		}
		if slug != current.Slug {
			// New slug: reset version to 1.0.0.
			targetVersion := "1.0.0"

			var exists bool
			err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM skills WHERE slug = ? AND id != ? AND version = ? AND deleted_at IS NULL)", slug, id, targetVersion)
			if err == nil && exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this version"})
				return
			}

			updates = append(updates, "slug = ?", "version = ?")
			args = append(args, slug, targetVersion)
		} else {
			// Same slug in payload: no version reset.
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

	if req.Metadata.Present {
		if req.Metadata.Value == nil {
			updates = append(updates, "metadata = ?")
			args = append(args, sql.NullString{String: "{}", Valid: true})
		} else {
			metadataJSON, err := json.Marshal(req.Metadata.Value)
			if err == nil {
				updates = append(updates, "metadata = ?")
				args = append(args, sql.NullString{String: string(metadataJSON), Valid: true})
			}
		}
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

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE skills SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	result, err := h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[SKILL] Failed to update skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update skill"})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
		return
	}

	// Fetch the updated skill
	var s skillRow
	err = h.db.Get(&s, "SELECT id, slug, description, version, metadata, created_at, updated_at FROM skills WHERE id = ?", id)
	if err != nil {
		logger.Printf("[SKILL] Failed to fetch updated skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated skill"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	version := "1.0.0"
	if s.Version.Valid {
		version = s.Version.String
	}
	var metadata interface{}
	if s.Metadata.Valid && s.Metadata.String != "" && s.Metadata.String != "null" {
		metadata = parseJSONRaw(s.Metadata.String)
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SkillResponse{
		ID:          fmt.Sprintf("%d", s.ID),
		Slug:        s.Slug,
		Description: description,
		Version:     version,
		Metadata:    metadata,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})
}

// DeleteSkill handles skill deletion requests (soft delete).
//
// @Summary      Delete skill
// @Description  Soft deletes a skill by ID
// @Tags         skills
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Skill ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /skills/{id} [delete]
func (h *SkillHandler) DeleteSkill(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid skill id"})
		return
	}

	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM skills WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[SKILL] Failed to check skill existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete skill"})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill not found"})
		return
	}

	// skill_sequence stores skill ids as JSON strings, e.g. ["1","2"].
	var referenced bool
	err = h.db.Get(&referenced, `
		SELECT EXISTS(
			SELECT 1 FROM sops
			WHERE deleted_at IS NULL
			  AND JSON_CONTAINS(skill_sequence, JSON_QUOTE(CAST(? AS CHAR)), '$')
		)
	`, id)
	if err != nil {
		logger.Printf("[SKILL] Failed to check SOP references: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete skill"})
		return
	}
	if referenced {
		c.JSON(http.StatusConflict, gin.H{"error": "skill is referenced by one or more SOPs"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE skills SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[SKILL] Failed to delete skill: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete skill"})
		return
	}

	c.Status(http.StatusNoContent)
}
