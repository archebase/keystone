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

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// SceneHandler handles scene related HTTP requests.
type SceneHandler struct {
	db *sqlx.DB
}

// NewSceneHandler creates a new SceneHandler.
func NewSceneHandler(db *sqlx.DB) *SceneHandler {
	return &SceneHandler{db: db}
}

// SceneResponse represents a scene in the response.
type SceneResponse struct {
	ID                         string `json:"id"`
	OrganizationID             string `json:"organization_id"`
	FactoryID                  string `json:"factory_id"`
	Name                       string `json:"name"`
	Slug                       string `json:"slug"`
	Description                string `json:"description,omitempty"`
	InitialSceneLayoutTemplate string `json:"initial_scene_layout_template,omitempty"`
	CreatedAt                  string `json:"created_at,omitempty"`
	UpdatedAt                  string `json:"updated_at,omitempty"`
}

// SceneListResponse represents the response for listing scenes.
type SceneListResponse struct {
	Scenes []SceneResponse `json:"scenes"`
}

// CreateSceneRequest represents the request body for creating a scene.
type CreateSceneRequest struct {
	OrganizationID             string `json:"organization_id"`
	FactoryID                  string `json:"factory_id"`
	Name                       string `json:"name"`
	Slug                       string `json:"slug"`
	Description                string `json:"description,omitempty"`
	InitialSceneLayoutTemplate string `json:"initial_scene_layout_template,omitempty"`
}

// CreateSceneResponse represents the response for creating a scene.
type CreateSceneResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"created_at"`
}

// UpdateSceneRequest represents the request body for updating a scene.
type UpdateSceneRequest struct {
	Name                       *string `json:"name,omitempty"`
	Slug                       *string `json:"slug,omitempty"`
	Description                *string `json:"description,omitempty"`
	InitialSceneLayoutTemplate *string `json:"initial_scene_layout_template,omitempty"`
}

// RegisterRoutes registers scene related routes.
func (h *SceneHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/scenes", h.ListScenes)
	apiV1.POST("/scenes", h.CreateScene)
	apiV1.GET("/scenes/:id", h.GetScene)
	apiV1.PUT("/scenes/:id", h.UpdateScene)
	apiV1.DELETE("/scenes/:id", h.DeleteScene)
}

// sceneRow represents a scene in the database
type sceneRow struct {
	ID                         int64          `db:"id"`
	OrganizationID             int64          `db:"organization_id"`
	FactoryID                  int64          `db:"factory_id"`
	Name                       string         `db:"name"`
	Slug                       string         `db:"slug"`
	Description                sql.NullString `db:"description"`
	InitialSceneLayoutTemplate sql.NullString `db:"initial_scene_layout_template"`
	CreatedAt                  sql.NullString `db:"created_at"`
	UpdatedAt                  sql.NullString `db:"updated_at"`
}

// ListScenes handles scene listing requests with filtering.
//
// @Summary      List scenes
// @Description  Lists scenes with optional filtering by organization_id and factory_id
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        organization_id query string false "Filter by organization ID"
// @Param        factory_id query string false "Filter by factory ID"
// @Success      200 {object} SceneListResponse
// @Failure      500 {object} map[string]string
// @Router       /scenes [get]
func (h *SceneHandler) ListScenes(c *gin.Context) {
	orgID := c.Query("organization_id")
	factoryID := c.Query("factory_id")

	query := `
		SELECT 
			id,
			organization_id,
			factory_id,
			name,
			slug,
			description,
			initial_scene_layout_template,
			created_at,
			updated_at
		FROM scenes
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

	if factoryID != "" {
		parsedFactoryID, err := strconv.ParseInt(factoryID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
			return
		}
		query += " AND factory_id = ?"
		args = append(args, parsedFactoryID)
	}

	query += " ORDER BY id DESC"

	var dbRows []sceneRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		logger.Printf("[SCENE] Failed to query scenes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list scenes"})
		return
	}

	scenes := []SceneResponse{}
	for _, s := range dbRows {
		description := ""
		if s.Description.Valid {
			description = s.Description.String
		}
		layoutTemplate := ""
		if s.InitialSceneLayoutTemplate.Valid {
			layoutTemplate = s.InitialSceneLayoutTemplate.String
		}
		createdAt := ""
		if s.CreatedAt.Valid {
			createdAt = s.CreatedAt.String
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.String
		}

		scenes = append(scenes, SceneResponse{
			ID:                         fmt.Sprintf("%d", s.ID),
			OrganizationID:             fmt.Sprintf("%d", s.OrganizationID),
			FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
			Name:                       s.Name,
			Slug:                       s.Slug,
			Description:                description,
			InitialSceneLayoutTemplate: layoutTemplate,
			CreatedAt:                  createdAt,
			UpdatedAt:                  updatedAt,
		})
	}

	c.JSON(http.StatusOK, SceneListResponse{
		Scenes: scenes,
	})
}

// GetScene handles getting a single scene by ID.
//
// @Summary      Get scene
// @Description  Gets a scene by ID
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Scene ID"
// @Success      200  {object}  SceneResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /scenes/{id} [get]
func (h *SceneHandler) GetScene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene id"})
		return
	}

	query := `
		SELECT 
			id,
			organization_id,
			factory_id,
			name,
			slug,
			description,
			initial_scene_layout_template,
			created_at,
			updated_at
		FROM scenes
		WHERE id = ? AND deleted_at IS NULL
	`

	var s sceneRow
	if err := h.db.Get(&s, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "scene not found"})
			return
		}
		logger.Printf("[SCENE] Failed to query scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get scene"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	layoutTemplate := ""
	if s.InitialSceneLayoutTemplate.Valid {
		layoutTemplate = s.InitialSceneLayoutTemplate.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SceneResponse{
		ID:                         fmt.Sprintf("%d", s.ID),
		OrganizationID:             fmt.Sprintf("%d", s.OrganizationID),
		FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
		Name:                       s.Name,
		Slug:                       s.Slug,
		Description:                description,
		InitialSceneLayoutTemplate: layoutTemplate,
		CreatedAt:                  createdAt,
		UpdatedAt:                  updatedAt,
	})
}

// CreateScene handles scene creation requests.
//
// @Summary      Create scene
// @Description  Creates a new scene
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        body  body      CreateSceneRequest  true  "Scene payload"
// @Success      201   {object}  CreateSceneResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /scenes [post]
func (h *SceneHandler) CreateScene(c *gin.Context) {
	var req CreateSceneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.OrganizationID = strings.TrimSpace(req.OrganizationID)
	req.FactoryID = strings.TrimSpace(req.FactoryID)
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(req.Slug)
	req.Description = strings.TrimSpace(req.Description)

	if req.OrganizationID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id is required"})
		return
	}

	if req.FactoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id is required"})
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

	// Parse factory_id
	factoryID, err := strconv.ParseInt(req.FactoryID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
		return
	}

	// Verify organization exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id = ? AND deleted_at IS NULL)", orgID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization not found"})
		return
	}

	// Verify factory exists
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
		return
	}

	// Check if slug already exists for this organization
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM scenes WHERE organization_id = ? AND slug = ? AND deleted_at IS NULL)", orgID, req.Slug)
	if err == nil && exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this organization"})
		return
	}

	var descriptionStr sql.NullString
	if req.Description != "" {
		descriptionStr = sql.NullString{String: req.Description, Valid: true}
	}

	var layoutTemplateStr sql.NullString
	if req.InitialSceneLayoutTemplate != "" {
		layoutTemplateStr = sql.NullString{String: req.InitialSceneLayoutTemplate, Valid: true}
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO scenes (
			organization_id,
			factory_id,
			name,
			slug,
			description,
			initial_scene_layout_template,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		orgID,
		factoryID,
		req.Name,
		req.Slug,
		descriptionStr,
		layoutTemplateStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[SCENE] Failed to insert scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create scene"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[SCENE] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create scene"})
		return
	}

	c.JSON(http.StatusCreated, CreateSceneResponse{
		ID:        fmt.Sprintf("%d", id),
		Name:      req.Name,
		Slug:      req.Slug,
		CreatedAt: now.Format(time.RFC3339),
	})
}

// UpdateScene handles updating a scene.
//
// @Summary      Update scene
// @Description  Updates an existing scene
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        id   path      string           true  "Scene ID"
// @Param        body body      UpdateSceneRequest  true  "Scene payload"
// @Success      200  {object}  SceneResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /scenes/{id} [put]
func (h *SceneHandler) UpdateScene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene id"})
		return
	}

	var req UpdateSceneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check if scene exists
	var existing sceneRow
	err = h.db.Get(&existing, "SELECT id, organization_id, factory_id, name, slug, description, initial_scene_layout_template, created_at, updated_at FROM scenes WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "scene not found"})
			return
		}
		logger.Printf("[SCENE] Failed to query scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update scene"})
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
			err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM scenes WHERE organization_id = ? AND slug = ? AND id != ? AND deleted_at IS NULL)", existing.OrganizationID, slug, id)
			if err == nil && exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this organization"})
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

	if req.InitialSceneLayoutTemplate != nil {
		layout := strings.TrimSpace(*req.InitialSceneLayoutTemplate)
		var layoutStr sql.NullString
		if layout != "" {
			layoutStr = sql.NullString{String: layout, Valid: true}
		}
		updates = append(updates, "initial_scene_layout_template = ?")
		args = append(args, layoutStr)
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE scenes SET %s WHERE id = ?", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[SCENE] Failed to update scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update scene"})
		return
	}

	// Fetch the updated scene
	var s sceneRow
	err = h.db.Get(&s, "SELECT id, organization_id, factory_id, name, slug, description, initial_scene_layout_template, created_at, updated_at FROM scenes WHERE id = ?", id)
	if err != nil {
		logger.Printf("[SCENE] Failed to fetch updated scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated scene"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	layoutTemplate := ""
	if s.InitialSceneLayoutTemplate.Valid {
		layoutTemplate = s.InitialSceneLayoutTemplate.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}

	c.JSON(http.StatusOK, SceneResponse{
		ID:                         fmt.Sprintf("%d", s.ID),
		OrganizationID:             fmt.Sprintf("%d", s.OrganizationID),
		FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
		Name:                       s.Name,
		Slug:                       s.Slug,
		Description:                description,
		InitialSceneLayoutTemplate: layoutTemplate,
		CreatedAt:                  createdAt,
		UpdatedAt:                  updatedAt,
	})
}

// DeleteScene handles scene deletion requests (soft delete).
//
// @Summary      Delete scene
// @Description  Soft deletes a scene by ID
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Scene ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /scenes/{id} [delete]
func (h *SceneHandler) DeleteScene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene id"})
		return
	}

	// Check if scene exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM scenes WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[SCENE] Failed to check scene existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scene"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "scene not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE scenes SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[SCENE] Failed to delete scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scene"})
		return
	}

	c.Status(http.StatusNoContent)
}
