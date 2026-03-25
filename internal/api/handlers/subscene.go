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

// SubsceneHandler handles subscene related HTTP requests.
type SubsceneHandler struct {
	db *sqlx.DB
}

// NewSubsceneHandler creates a new SubsceneHandler.
func NewSubsceneHandler(db *sqlx.DB) *SubsceneHandler {
	return &SubsceneHandler{db: db}
}

// SubsceneResponse represents a subscene in the response.
type SubsceneResponse struct {
	ID                 string `json:"id"`
	SceneID            string `json:"scene_id"`
	Name               string `json:"name"`
	Slug               string `json:"slug,omitempty"`
	Description        string `json:"description,omitempty"`
	InitialSceneLayout string `json:"initial_scene_layout,omitempty"`
	RobotTypeID        string `json:"robot_type_id,omitempty"`
	CreatedAt          string `json:"created_at,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
}

// SubsceneListResponse represents the response for listing subscenes.
type SubsceneListResponse struct {
	Subscenes []SubsceneResponse `json:"subscenes"`
}

// CreateSubsceneRequest represents the request body for creating a subscene.
type CreateSubsceneRequest struct {
	SceneID            string  `json:"scene_id"`
	Name               string  `json:"name"`
	Slug               *string `json:"slug,omitempty"`
	Description        string  `json:"description,omitempty"`
	InitialSceneLayout string  `json:"initial_scene_layout,omitempty"`
	RobotTypeID        *string `json:"robot_type_id,omitempty"`
}

// CreateSubsceneResponse represents the response for creating a subscene.
type CreateSubsceneResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug,omitempty"`
	CreatedAt string `json:"created_at"`
}

// UpdateSubsceneRequest represents the request body for updating a subscene.
type UpdateSubsceneRequest struct {
	Name               *string `json:"name,omitempty"`
	Slug               *string `json:"slug,omitempty"`
	Description        *string `json:"description,omitempty"`
	InitialSceneLayout *string `json:"initial_scene_layout,omitempty"`
	RobotTypeID        *string `json:"robot_type_id,omitempty"`
}

// RegisterRoutes registers subscene related routes.
func (h *SubsceneHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/subscenes", h.ListSubscenes)
	apiV1.POST("/subscenes", h.CreateSubscene)
	apiV1.GET("/subscenes/:id", h.GetSubscene)
	apiV1.PUT("/subscenes/:id", h.UpdateSubscene)
	apiV1.DELETE("/subscenes/:id", h.DeleteSubscene)
}

// subsceneRow represents a subscene in the database
type subsceneRow struct {
	ID                 int64          `db:"id"`
	SceneID            int64          `db:"scene_id"`
	Name               string         `db:"name"`
	Slug               sql.NullString `db:"slug"`
	Description        sql.NullString `db:"description"`
	InitialSceneLayout sql.NullString `db:"initial_scene_layout"`
	RobotTypeID        sql.NullInt64  `db:"robot_type_id"`
	CreatedAt          sql.NullString `db:"created_at"`
	UpdatedAt          sql.NullString `db:"updated_at"`
}

// ListSubscenes handles subscene listing requests with filtering.
//
// @Summary      List subscenes
// @Description  Lists subscenes with optional filtering by scene_id
// @Tags         subscenes
// @Accept       json
// @Produce      json
// @Param        scene_id query string false "Filter by scene ID"
// @Success      200 {object} SubsceneListResponse
// @Failure      500 {object} map[string]string
// @Router       /subscenes [get]
func (h *SubsceneHandler) ListSubscenes(c *gin.Context) {
	sceneID := c.Query("scene_id")

	query := `
		SELECT 
			id,
			scene_id,
			name,
			slug,
			description,
			initial_scene_layout,
			robot_type_id,
			created_at,
			updated_at
		FROM subscenes
		WHERE deleted_at IS NULL
	`
	args := []interface{}{}

	if sceneID != "" {
		parsedSceneID, err := strconv.ParseInt(sceneID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene_id format"})
			return
		}
		query += " AND scene_id = ?"
		args = append(args, parsedSceneID)
	}

	query += " ORDER BY id DESC"

	var dbRows []subsceneRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		logger.Printf("[SUBSCENE] Failed to query subscenes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list subscenes"})
		return
	}

	subscenes := []SubsceneResponse{}
	for _, s := range dbRows {
		description := ""
		if s.Description.Valid {
			description = s.Description.String
		}
		layout := ""
		if s.InitialSceneLayout.Valid {
			layout = s.InitialSceneLayout.String
		}
		createdAt := ""
		if s.CreatedAt.Valid {
			createdAt = s.CreatedAt.String
		}
		updatedAt := ""
		if s.UpdatedAt.Valid {
			updatedAt = s.UpdatedAt.String
		}
		slug := ""
		if s.Slug.Valid {
			slug = s.Slug.String
		}
		robotTypeID := ""
		if s.RobotTypeID.Valid {
			robotTypeID = fmt.Sprintf("%d", s.RobotTypeID.Int64)
		}

		subscenes = append(subscenes, SubsceneResponse{
			ID:                 fmt.Sprintf("%d", s.ID),
			SceneID:            fmt.Sprintf("%d", s.SceneID),
			Name:               s.Name,
			Slug:               slug,
			Description:        description,
			InitialSceneLayout: layout,
			RobotTypeID:        robotTypeID,
			CreatedAt:          createdAt,
			UpdatedAt:          updatedAt,
		})
	}

	c.JSON(http.StatusOK, SubsceneListResponse{
		Subscenes: subscenes,
	})
}

// GetSubscene handles getting a single subscene by ID.
//
// @Summary      Get subscene
// @Description  Gets a subscene by ID
// @Tags         subscenes
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "Subscene ID"
// @Success      200  {object}  SubsceneResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /subscenes/{id} [get]
func (h *SubsceneHandler) GetSubscene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid subscene id"})
		return
	}

	query := `
		SELECT 
			id,
			scene_id,
			name,
			slug,
			description,
			initial_scene_layout,
			robot_type_id,
			created_at,
			updated_at
		FROM subscenes
		WHERE id = ? AND deleted_at IS NULL
	`

	var s subsceneRow
	if err := h.db.Get(&s, query, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "subscene not found"})
			return
		}
		logger.Printf("[SUBSCENE] Failed to query subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get subscene"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	layout := ""
	if s.InitialSceneLayout.Valid {
		layout = s.InitialSceneLayout.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}
	slug := ""
	if s.Slug.Valid {
		slug = s.Slug.String
	}
	robotTypeID := ""
	if s.RobotTypeID.Valid {
		robotTypeID = fmt.Sprintf("%d", s.RobotTypeID.Int64)
	}

	c.JSON(http.StatusOK, SubsceneResponse{
		ID:                 fmt.Sprintf("%d", s.ID),
		SceneID:            fmt.Sprintf("%d", s.SceneID),
		Name:               s.Name,
		Slug:               slug,
		Description:        description,
		InitialSceneLayout: layout,
		RobotTypeID:        robotTypeID,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	})
}

// CreateSubscene handles subscene creation requests.
//
// @Summary      Create subscene
// @Description  Creates a new subscene
// @Tags         subscenes
// @Accept       json
// @Produce      json
// @Param        body  body      CreateSubsceneRequest  true  "Subscene payload"
// @Success      201   {object}  CreateSubsceneResponse
// @Failure      400   {object}  map[string]string
// @Failure      500   {object}  map[string]string
// @Router       /subscenes [post]
func (h *SubsceneHandler) CreateSubscene(c *gin.Context) {
	var req CreateSubsceneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	req.SceneID = strings.TrimSpace(req.SceneID)
	req.Name = strings.TrimSpace(req.Name)
	if req.Slug != nil {
		trimmed := strings.TrimSpace(*req.Slug)
		req.Slug = &trimmed
	}
	req.Description = strings.TrimSpace(req.Description)
	if req.RobotTypeID != nil {
		trimmed := strings.TrimSpace(*req.RobotTypeID)
		req.RobotTypeID = &trimmed
	}

	if req.SceneID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scene_id is required"})
		return
	}

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	// Parse scene_id
	sceneID, err := strconv.ParseInt(req.SceneID, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid scene_id format"})
		return
	}

	// Verify scene exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM scenes WHERE id = ? AND deleted_at IS NULL)", sceneID)
	if err != nil || !exists {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scene not found"})
		return
	}

	// Parse and verify robot_type_id (optional)
	var robotTypeID sql.NullInt64
	if req.RobotTypeID != nil && *req.RobotTypeID != "" {
		parsedRobotTypeID, err := strconv.ParseInt(*req.RobotTypeID, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
			return
		}
		err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", parsedRobotTypeID)
		if err != nil || !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type not found"})
			return
		}
		robotTypeID = sql.NullInt64{Int64: parsedRobotTypeID, Valid: true}
	}

	// Check if slug already exists for this scene (if slug is provided)
	if req.Slug != nil && *req.Slug != "" {
		err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM subscenes WHERE scene_id = ? AND slug = ? AND deleted_at IS NULL)", sceneID, *req.Slug)
		if err == nil && exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this scene"})
			return
		}
	}

	var descriptionStr sql.NullString
	if req.Description != "" {
		descriptionStr = sql.NullString{String: req.Description, Valid: true}
	}

	var layoutStr sql.NullString
	if req.InitialSceneLayout != "" {
		layoutStr = sql.NullString{String: req.InitialSceneLayout, Valid: true}
	}

	var slugStr sql.NullString
	if req.Slug != nil && *req.Slug != "" {
		slugStr = sql.NullString{String: *req.Slug, Valid: true}
	}

	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO subscenes (
			scene_id,
			name,
			slug,
			description,
			initial_scene_layout,
			robot_type_id,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sceneID,
		req.Name,
		slugStr,
		descriptionStr,
		layoutStr,
		robotTypeID,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to insert subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create subscene"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create subscene"})
		return
	}

	c.JSON(http.StatusCreated, CreateSubsceneResponse{
		ID:   fmt.Sprintf("%d", id),
		Name: req.Name,
		Slug: func() string {
			if req.Slug != nil {
				return *req.Slug
			}
			return ""
		}(),
		CreatedAt: now.Format(time.RFC3339),
	})
}

// UpdateSubscene handles updating a subscene.
//
// @Summary      Update subscene
// @Description  Updates an existing subscene
// @Tags         subscenes
// @Accept       json
// @Produce      json
// @Param        id   path      string             true  "Subscene ID"
// @Param        body body      UpdateSubsceneRequest  true  "Subscene payload"
// @Success      200  {object}  SubsceneResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /subscenes/{id} [put]
func (h *SubsceneHandler) UpdateSubscene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid subscene id"})
		return
	}

	var req UpdateSubsceneRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Check if subscene exists
	var existing subsceneRow
	err = h.db.Get(&existing, "SELECT id, scene_id, name, slug, description, initial_scene_layout, robot_type_id, created_at, updated_at FROM subscenes WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "subscene not found"})
			return
		}
		logger.Printf("[SUBSCENE] Failed to query subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update subscene"})
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
			// Check if slug already exists for this scene
			var exists bool
			err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM subscenes WHERE scene_id = ? AND slug = ? AND id != ? AND deleted_at IS NULL)", existing.SceneID, slug, id)
			if err == nil && exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "slug already exists for this scene"})
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

	if req.InitialSceneLayout != nil {
		layout := strings.TrimSpace(*req.InitialSceneLayout)
		var layoutStr sql.NullString
		if layout != "" {
			layoutStr = sql.NullString{String: layout, Valid: true}
		}
		updates = append(updates, "initial_scene_layout = ?")
		args = append(args, layoutStr)
	}

	// Handle robot_type_id update
	if req.RobotTypeID != nil {
		if *req.RobotTypeID == "" {
			// Set to NULL to remove association
			updates = append(updates, "robot_type_id = ?")
			args = append(args, sql.NullInt64{})
		} else {
			parsedRobotTypeID, err := strconv.ParseInt(*req.RobotTypeID, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id format"})
				return
			}
			// Verify robot_type exists
			var exists bool
			err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", parsedRobotTypeID)
			if err != nil || !exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "robot_type not found"})
				return
			}
			updates = append(updates, "robot_type_id = ?")
			args = append(args, sql.NullInt64{Int64: parsedRobotTypeID, Valid: true})
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

	query := fmt.Sprintf("UPDATE subscenes SET %s WHERE id = ?", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to update subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update subscene"})
		return
	}

	// Fetch the updated subscene
	var s subsceneRow
	err = h.db.Get(&s, "SELECT id, scene_id, name, slug, description, initial_scene_layout, robot_type_id, created_at, updated_at FROM subscenes WHERE id = ?", id)
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to fetch updated subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated subscene"})
		return
	}

	description := ""
	if s.Description.Valid {
		description = s.Description.String
	}
	layout := ""
	if s.InitialSceneLayout.Valid {
		layout = s.InitialSceneLayout.String
	}
	createdAt := ""
	if s.CreatedAt.Valid {
		createdAt = s.CreatedAt.String
	}
	updatedAt := ""
	if s.UpdatedAt.Valid {
		updatedAt = s.UpdatedAt.String
	}
	slug := ""
	if s.Slug.Valid {
		slug = s.Slug.String
	}
	robotTypeID := ""
	if s.RobotTypeID.Valid {
		robotTypeID = fmt.Sprintf("%d", s.RobotTypeID.Int64)
	}

	c.JSON(http.StatusOK, SubsceneResponse{
		ID:                 fmt.Sprintf("%d", s.ID),
		SceneID:            fmt.Sprintf("%d", s.SceneID),
		Name:               s.Name,
		Slug:               slug,
		Description:        description,
		InitialSceneLayout: layout,
		RobotTypeID:        robotTypeID,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
	})
}

// DeleteSubscene handles subscene deletion requests (soft delete).
//
// @Summary      Delete subscene
// @Description  Soft deletes a subscene by ID
// @Tags         subscenes
// @Accept       json
// @Produce      json
// @Param        id path     string  true  "Subscene ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /subscenes/{id} [delete]
func (h *SubsceneHandler) DeleteSubscene(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid subscene id"})
		return
	}

	// Check if subscene exists
	var exists bool
	err = h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM subscenes WHERE id = ? AND deleted_at IS NULL)", id)
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to check subscene existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete subscene"})
		return
	}

	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscene not found"})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE subscenes SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	if err != nil {
		logger.Printf("[SUBSCENE] Failed to delete subscene: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete subscene"})
		return
	}

	c.Status(http.StatusNoContent)
}
