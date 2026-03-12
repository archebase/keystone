// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
}

// CreateFactoryResponse represents the response for creating a factory.
type CreateFactoryResponse struct {
	ID             string `json:"id"`
	OrganizationID string `json:"organization_id"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	CreatedAt      string `json:"created_at"`
}

// RegisterRoutes registers factory related routes.
func (h *FactoryHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/factories", h.ListFactories)
	apiV1.POST("/factories", h.CreateFactory)
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
		query += " AND organization_id = ?"
		args = append(args, orgID)
	}

	query += " ORDER BY id DESC"

	rows, err := h.db.Queryx(query, args...)
	if err != nil {
		log.Printf("[ListFactories] Failed to query factories: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list factories"})
		return
	}
	defer func() {
		if err := rows.Close(); err != nil {
			log.Printf("[ListFactories] Failed to close rows: %v", err)
		}
	}()

	factories := []FactoryResponse{}
	for rows.Next() {
		var f struct {
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

		if err := rows.StructScan(&f); err != nil {
			log.Printf("[ListFactories] Failed to scan factory: %v", err)
			continue
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

	result, err := h.db.Exec(
		`INSERT INTO factories (
			organization_id,
			name,
			slug,
			timezone,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		orgID,
		req.Name,
		req.Slug,
		"UTC",
		now,
		now,
	)
	if err != nil {
		log.Printf("[CreateFactory] Failed to insert factory: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		log.Printf("[CreateFactory] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create factory"})
		return
	}

	c.JSON(http.StatusCreated, CreateFactoryResponse{
		ID:             fmt.Sprintf("%d", id),
		OrganizationID: req.OrganizationID,
		Name:           req.Name,
		Slug:           req.Slug,
		CreatedAt:      now.Format(time.RFC3339),
	})
}
