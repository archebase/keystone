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
	FactoryID                  string `json:"factory_id"`
	Name                       string `json:"name"`
	Description                string `json:"description,omitempty"`
	InitialSceneLayoutTemplate string `json:"initial_scene_layout_template,omitempty"`
	SubsceneCount              int    `json:"subsceneCount"`
	CreatedAt                  string `json:"created_at,omitempty"`
	UpdatedAt                  string `json:"updated_at,omitempty"`
}

// SceneListResponse represents the response for listing scenes.
type SceneListResponse struct {
	Items   []SceneResponse `json:"items"`
	Total   int             `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	HasNext bool            `json:"hasNext,omitempty"`
	HasPrev bool            `json:"hasPrev,omitempty"`
}

// CreateSceneRequest represents the request body for creating a scene.
type CreateSceneRequest struct {
	FactoryID                  string `json:"factory_id"`
	Name                       string `json:"name"`
	Description                string `json:"description,omitempty"`
	InitialSceneLayoutTemplate string `json:"initial_scene_layout_template,omitempty"`
}

// CreateSceneResponse represents the response for creating a scene.
type CreateSceneResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// UpdateSceneRequest represents the request body for updating a scene.
type UpdateSceneRequest struct {
	FactoryID                  *string `json:"factory_id,omitempty"`
	Name                       *string `json:"name,omitempty"`
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
	FactoryID                  int64          `db:"factory_id"`
	Name                       string         `db:"name"`
	Description                sql.NullString `db:"description"`
	InitialSceneLayoutTemplate sql.NullString `db:"initial_scene_layout_template"`
	SubsceneCount              int            `db:"subscene_count"`
	CreatedAt                  sql.NullTime   `db:"created_at"`
	UpdatedAt                  sql.NullTime   `db:"updated_at"`
}

// ListScenes handles scene listing requests with filtering.
//
// @Summary      List scenes
// @Description  Lists scenes with optional filtering by factory_id
// @Tags         scenes
// @Accept       json
// @Produce      json
// @Param        factory_id query string false "Filter by factory ID(s), comma-separated"
// @Param        scene_id   query string false "Filter by scene ID(s), comma-separated"
// @Param        id         query string false "Alias of scene_id"
// @Param        keyword    query string false "Search by name or description"
// @Param        q          query string false "Alias of keyword"
// @Param        search     query string false "Alias of keyword"
// @Param        limit      query int    false "Max results (default 50, max 100)"
// @Param        offset     query int    false "Pagination offset (default 0)"
// @Success      200 {object} SceneListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /scenes [get]
func (h *SceneHandler) ListScenes(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	factoryIDs, err := parsePositiveInt64List(c.Query("factory_id"), "factory_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	sceneIDs, err := parsePositiveInt64List(firstNonEmptyQuery(c, "scene_id", "id"), "scene_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")

	whereClause := "WHERE s.deleted_at IS NULL"
	args := []any{}

	whereClause, args = appendInt64InFilter(whereClause, args, "s.factory_id", factoryIDs)
	whereClause, args = appendInt64InFilter(whereClause, args, "s.id", sceneIDs)
	whereClause, args = appendKeywordSearch(whereClause, args, keyword, "s.name", "s.description")

	countQuery := "SELECT COUNT(*) FROM scenes s " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[SCENE] Failed to count scenes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list scenes"})
		return
	}

	orderClause, orderArgs := keywordOrderBy(keyword, "s.id DESC", "s.name", "s.description")
	query := `
		SELECT 
			s.id,
			s.factory_id,
			s.name,
			s.description,
			s.initial_scene_layout_template,
			s.created_at,
			s.updated_at,
			(SELECT COUNT(*) FROM subscenes sub WHERE sub.scene_id = s.id AND sub.deleted_at IS NULL) as subscene_count
		FROM scenes s
		` + whereClause + `
		` + orderClause + `
		LIMIT ? OFFSET ?
	`

	queryArgs := append(args, orderArgs...)
	queryArgs = append(queryArgs, pagination.Limit, pagination.Offset)

	var dbRows []sceneRow
	if err := h.db.Select(&dbRows, query, queryArgs...); err != nil {
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
			createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}
		scenes = append(scenes, SceneResponse{
			ID:                         fmt.Sprintf("%d", s.ID),
			FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
			Name:                       s.Name,
			Description:                description,
			InitialSceneLayoutTemplate: layoutTemplate,
			SubsceneCount:              s.SubsceneCount,
			CreatedAt:                  createdAt,
			UpdatedAt:                  updatedAt,
		})
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, SceneListResponse{
		Items:   scenes,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
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
			factory_id,
			name,
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
		createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, SceneResponse{
		ID:                         fmt.Sprintf("%d", s.ID),
		FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
		Name:                       s.Name,
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

	req.FactoryID = strings.TrimSpace(req.FactoryID)
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)

	if req.FactoryID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id is required"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	// Parse factory_id
	factoryID, err := strconv.ParseInt(req.FactoryID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
		return
	}

	// Verify factory exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID)
	if err != nil {
		logger.Printf("[SCENE] Failed to verify factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create scene"})
		return
	}
	if !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
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
			factory_id,
			name,
			description,
			initial_scene_layout_template,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		factoryID,
		req.Name,
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
	err = h.db.Get(&existing, "SELECT id, factory_id, name, description, initial_scene_layout_template, created_at, updated_at FROM scenes WHERE id = ? AND deleted_at IS NULL", id)
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

	if req.FactoryID != nil {
		factoryIDStr := strings.TrimSpace(*req.FactoryID)
		if factoryIDStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "factory_id cannot be empty"})
			return
		}
		factoryID, err := strconv.ParseInt(factoryIDStr, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid factory_id format"})
			return
		}
		// Verify factory exists
		var exists bool
		err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM factories WHERE id = ? AND deleted_at IS NULL)", factoryID)
		if err != nil || !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "factory not found"})
			return
		}
		updates = append(updates, "factory_id = ?")
		args = append(args, factoryID)
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name != "" {
			updates = append(updates, "name = ?")
			args = append(args, name)
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

	query := fmt.Sprintf("UPDATE scenes SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[SCENE] Failed to update scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update scene"})
		return
	}

	// Fetch the updated scene
	var s sceneRow
	err = h.db.Get(&s, "SELECT id, factory_id, name, description, initial_scene_layout_template, created_at, updated_at FROM scenes WHERE id = ?", id)
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
		createdAt = s.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	c.JSON(http.StatusOK, SceneResponse{
		ID:                         fmt.Sprintf("%d", s.ID),
		FactoryID:                  fmt.Sprintf("%d", s.FactoryID),
		Name:                       s.Name,
		Description:                description,
		InitialSceneLayoutTemplate: layoutTemplate,
		CreatedAt:                  createdAt,
		UpdatedAt:                  updatedAt,
	})
}

// DeleteScene handles scene deletion requests (soft delete).
//
// @Summary      Delete scene
// @Description  Soft deletes a scene by ID. Returns 400 if the scene has associated subscenes.
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

	// Check if scene has associated subscenes
	var subsceneCount int
	err = h.db.Get(&subsceneCount, "SELECT COUNT(*) FROM subscenes WHERE scene_id = ? AND deleted_at IS NULL", id)
	if err != nil {
		logger.Printf("[SCENE] Failed to check subscene count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scene"})
		return
	}

	if subsceneCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete scene with %d associated subscenes", subsceneCount)})
		return
	}

	// Check if scene is referenced by any orders
	var orderCount int
	err = h.db.Get(&orderCount, "SELECT COUNT(*) FROM orders WHERE scene_id = ? AND deleted_at IS NULL", id)
	if err != nil {
		logger.Printf("[SCENE] Failed to check order references: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scene"})
		return
	}
	if orderCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete scene referenced by %d orders", orderCount)})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE scenes SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[SCENE] Failed to delete scene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete scene"})
		return
	}

	c.Status(http.StatusNoContent)
}
