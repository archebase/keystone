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
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Slug       string      `json:"slug"`
	Location   string      `json:"location,omitempty"`
	Timezone   string      `json:"timezone,omitempty"`
	Settings   interface{} `json:"settings"`
	SceneCount int         `json:"sceneCount"`
	OrgCount   int         `json:"orgCount"`
	CreatedAt  string      `json:"created_at,omitempty"`
	UpdatedAt  string      `json:"updated_at,omitempty"`
}

// FactoryListResponse represents the response for listing factories.
type FactoryListResponse struct {
	Factories []FactoryResponse `json:"factories"`
}

// CreateFactoryRequest represents the request body for creating a factory.
// The server assigns a unique slug (fac- plus 10 random [a-z0-9] characters).
type CreateFactoryRequest struct {
	Name     string      `json:"name"`
	Location string      `json:"location,omitempty"`
	Timezone string      `json:"timezone,omitempty"`
	Settings interface{} `json:"settings,omitempty"`
}

// CreateFactoryResponse represents the response for creating a factory.
type CreateFactoryResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Location  string `json:"location,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	CreatedAt string `json:"created_at"`
}

// RegisterRoutes registers factory related routes.
func (h *FactoryHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/factories", h.ListFactories)
	apiV1.POST("/factories", h.CreateFactory)
	apiV1.GET("/factories/:id", h.GetFactory)
	apiV1.PUT("/factories/:id", h.UpdateFactory)
	apiV1.DELETE("/factories/:id", h.DeleteFactory)
}

// factoryRow represents a factory in the database
type factoryRow struct {
	ID         int64          `db:"id"`
	Name       string         `db:"name"`
	Slug       string         `db:"slug"`
	Location   sql.NullString `db:"location"`
	Timezone   sql.NullString `db:"timezone"`
	Settings   sql.NullString `db:"settings"`
	SceneCount int            `db:"scene_count"`
	OrgCount   int            `db:"org_count"`
	CreatedAt  sql.NullTime   `db:"created_at"`
	UpdatedAt  sql.NullTime   `db:"updated_at"`
}

func factorySettingsFromDB(ns sql.NullString) interface{} {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return nil
	}
	return json.RawMessage(ns.String)
}

const (
	factorySlugPrefix   = "fac-"
	factorySlugRandLen  = 10
	factorySlugRetries  = 20
	factorySlugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func randomFactorySlugSuffix() (string, error) {
	raw := make([]byte, factorySlugRandLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, factorySlugRandLen)
	for i := range out {
		out[i] = factorySlugAlphabet[int(raw[i])%len(factorySlugAlphabet)]
	}
	return string(out), nil
}

func (h *FactoryHandler) allocateFactorySlug() (string, error) {
	for i := 0; i < factorySlugRetries; i++ {
		suffix, err := randomFactorySlugSuffix()
		if err != nil {
			return "", err
		}
		slug := factorySlugPrefix + suffix
		var exists bool
		if err := h.db.Get(&exists,
			"SELECT EXISTS(SELECT 1 FROM factories WHERE slug = ? AND deleted_at IS NULL)", slug); err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
	}
	return "", fmt.Errorf("could not allocate unique factory slug")
}

// ListFactories handles factory listing requests with filtering and pagination.
//
// @Summary      List factories
// @Description  Lists all factories
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        limit  query     int     false  "Max results (default 50, max 100)"
// @Param        offset query     int     false  "Pagination offset (default 0)"
// @Success      200    {object}  ListResponse
// @Failure      400    {object}  map[string]string
// @Failure      500    {object}  map[string]string
// @Router       /factories [get]
func (h *FactoryHandler) ListFactories(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	countQuery := "SELECT COUNT(*) FROM factories WHERE deleted_at IS NULL"
	var total int
	if err := h.db.Get(&total, countQuery); err != nil {
		logger.Printf("[FACTORY] Failed to count factories: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list factories"})
		return
	}

	query := `
		SELECT 
			id,
			name,
			slug,
			location,
			timezone,
			settings,
			(SELECT COUNT(*) FROM scenes s WHERE s.factory_id = factories.id AND s.deleted_at IS NULL) AS scene_count,
			(SELECT COUNT(*) FROM organizations o WHERE o.factory_id = factories.id AND o.deleted_at IS NULL) AS org_count,
			created_at,
			updated_at
		FROM factories
		WHERE deleted_at IS NULL
		ORDER BY id DESC
		LIMIT ? OFFSET ?
	`

	factories := []FactoryResponse{}

	var dbRows []factoryRow
	if err := h.db.Select(&dbRows, query, pagination.Limit, pagination.Offset); err != nil {
		logger.Printf("[FACTORY] Failed to query factories: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list factories"})
		return
	}

	for _, f := range dbRows {
		factories = append(factories, factoryResponseFromRow(f))
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, ListResponse{
		Items:   factories,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// CreateFactory handles factory creation requests.
//
// @Summary      Create factory
// @Description  Creates a new factory. The server assigns a unique fac- prefix plus 10 random characters as slug. Factory names must be unique among non-deleted rows.
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

	req.Name = strings.TrimSpace(req.Name)
	req.Location = strings.TrimSpace(req.Location)
	req.Timezone = strings.TrimSpace(req.Timezone)

	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	var nameTaken bool
	if err := h.db.Get(&nameTaken,
		"SELECT EXISTS(SELECT 1 FROM factories WHERE name = ? AND deleted_at IS NULL)", req.Name); err != nil {
		logger.Printf("[FACTORY] Failed to check factory name: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
		return
	}
	if nameTaken {
		c.JSON(http.StatusBadRequest, gin.H{"error": "factory name already exists"})
		return
	}

	slug, err := h.allocateFactorySlug()
	if err != nil {
		logger.Printf("[FACTORY] Failed to allocate slug: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
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
	timezoneStr := sql.NullString{String: timezone, Valid: true}

	// Convert settings to JSON string if provided
	var settingsStr sql.NullString
	if req.Settings != nil {
		settingsJSON, err := json.Marshal(req.Settings)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settings"})
			return
		}
		settingsStr = sql.NullString{String: string(settingsJSON), Valid: true}
	}

	result, err := h.db.Exec(
		`INSERT INTO factories (
			name,
			slug,
			location,
			timezone,
			settings,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.Name,
		slug,
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
		ID:        fmt.Sprintf("%d", id),
		Name:      req.Name,
		Slug:      slug,
		Location:  req.Location,
		Timezone:  timezone,
		CreatedAt: now.Format(time.RFC3339),
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

	var f factoryRow
	if err := h.db.Get(&f, `
		SELECT id, name, slug, location, timezone, settings,
			(SELECT COUNT(*) FROM scenes s WHERE s.factory_id = factories.id AND s.deleted_at IS NULL) AS scene_count,
			(SELECT COUNT(*) FROM organizations o WHERE o.factory_id = factories.id AND o.deleted_at IS NULL) AS org_count,
			created_at, updated_at
		FROM factories
		WHERE id = ? AND deleted_at IS NULL
	`, id); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "factory not found"})
			return
		}
		logger.Printf("[FACTORY] Failed to query factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get factory"})
		return
	}

	c.JSON(http.StatusOK, factoryResponseFromRow(f))
}

// UpdateFactoryRequest represents the request body for updating a factory.
// Factory slug is immutable after creation; a slug field in JSON, if sent, is ignored.
type UpdateFactoryRequest struct {
	Name     *string                   `json:"name,omitempty"`
	Location *string                   `json:"location,omitempty"`
	Timezone *string                   `json:"timezone,omitempty"`
	Settings organizationSettingsPatch `json:"settings,omitempty"`
}

// UpdateFactory handles updating a factory.
//
// @Summary      Update factory
// @Description  Updates an existing factory. Factory slug cannot be changed after creation.
// @Tags         factories
// @Accept       json
// @Produce      json
// @Param        id   path      string                true  "Factory ID"
// @Param        body body      UpdateFactoryRequest  true  "Factory payload"
// @Success      200  {object}  FactoryResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /factories/{id} [put]
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
	err = h.db.Get(&existing, "SELECT id, name, slug, location, timezone, settings, created_at, updated_at FROM factories WHERE id = ? AND deleted_at IS NULL", id)
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
			var taken bool
			if err := h.db.Get(&taken,
				"SELECT EXISTS(SELECT 1 FROM factories WHERE name = ? AND id != ? AND deleted_at IS NULL)", name, id); err != nil {
				logger.Printf("[FACTORY] Failed to check factory name: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update factory"})
				return
			}
			if taken {
				c.JSON(http.StatusBadRequest, gin.H{"error": "factory name already exists"})
				return
			}
			updates = append(updates, "name = ?")
			args = append(args, name)
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

	query := fmt.Sprintf("UPDATE factories SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))

	_, err = h.db.Exec(query, args...)
	if err != nil {
		logger.Printf("[FACTORY] Failed to update factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update factory"})
		return
	}

	// Fetch the updated factory
	var f factoryRow
	err = h.db.Get(&f, `
		SELECT id, name, slug, location, timezone, settings,
			(SELECT COUNT(*) FROM scenes s WHERE s.factory_id = factories.id AND s.deleted_at IS NULL) AS scene_count,
			(SELECT COUNT(*) FROM organizations o WHERE o.factory_id = factories.id AND o.deleted_at IS NULL) AS org_count,
			created_at, updated_at
		FROM factories WHERE id = ?`, id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to fetch updated factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated factory"})
		return
	}

	c.JSON(http.StatusOK, factoryResponseFromRow(f))
}

// DeleteFactory handles factory deletion requests (soft delete).
//
// @Summary      Delete factory
// @Description  Soft deletes a factory by ID. Returns 400 if the factory has associated scenes or robots.
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

	// Check if factory has associated scenes
	var sceneCount int
	err = h.db.Get(&sceneCount, "SELECT COUNT(*) FROM scenes WHERE factory_id = ? AND deleted_at IS NULL", id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to check scene count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete factory"})
		return
	}

	if sceneCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete factory with %d associated scenes", sceneCount)})
		return
	}

	var robotCount int
	err = h.db.Get(&robotCount, "SELECT COUNT(*) FROM robots WHERE factory_id = ? AND deleted_at IS NULL", id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to check robot count: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete factory"})
		return
	}

	if robotCount > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("cannot delete factory with %d associated robots", robotCount)})
		return
	}

	now := time.Now().UTC()

	// Perform soft delete by setting deleted_at
	_, err = h.db.Exec("UPDATE factories SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[FACTORY] Failed to delete factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete factory"})
		return
	}

	c.Status(http.StatusNoContent)
}

// factoryResponseFromRow converts a factoryRow to a FactoryResponse.
func factoryResponseFromRow(f factoryRow) FactoryResponse {
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
		createdAt = f.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	updatedAt := ""
	if f.UpdatedAt.Valid {
		updatedAt = f.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	return FactoryResponse{
		ID:         fmt.Sprintf("%d", f.ID),
		Name:       f.Name,
		Slug:       f.Slug,
		Location:   location,
		Timezone:   timezone,
		Settings:   factorySettingsFromDB(f.Settings),
		SceneCount: f.SceneCount,
		OrgCount:   f.OrgCount,
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
}
