// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

const (
	defaultWorkspaceID          int64  = 0
	defaultWorkspaceName        string = "Default Workspace"
	defaultWorkspaceDescription string = "Local-only fallback workspace"

	workspaceSourceDefault string = "default"
	workspaceSourceHilbert string = "hilbert"
)

// WorkspaceHandler handles workspace related HTTP requests.
type WorkspaceHandler struct {
	db          *sqlx.DB
	syncService *services.WorkspaceSyncService
}

// NewWorkspaceHandler creates a new WorkspaceHandler.
func NewWorkspaceHandler(db *sqlx.DB, syncService ...*services.WorkspaceSyncService) *WorkspaceHandler {
	var service *services.WorkspaceSyncService
	if len(syncService) > 0 {
		service = syncService[0]
	}
	return &WorkspaceHandler{db: db, syncService: service}
}

// WorkspaceResponse represents a workspace in the response.
type WorkspaceResponse struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Source       string   `json:"source"`
	Admins       []string `json:"admins"`
	Members      []string `json:"members"`
	LastSyncedAt string   `json:"last_synced_at,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
}

// WorkspaceListResponse represents the response for listing workspaces.
type WorkspaceListResponse struct {
	Items   []WorkspaceResponse `json:"items"`
	Total   int                 `json:"total"`
	Limit   int                 `json:"limit"`
	Offset  int                 `json:"offset"`
	HasNext bool                `json:"hasNext,omitempty"`
	HasPrev bool                `json:"hasPrev,omitempty"`
}

// WorkspaceSyncResponse represents a manual workspace sync result.
type WorkspaceSyncResponse struct {
	SyncedCount     int                                    `json:"synced_count"`
	DefaultIncluded bool                                   `json:"default_included"`
	LastSyncedAt    string                                 `json:"last_synced_at"`
	ResourceSync    *services.WorkspaceResourceSyncSummary `json:"resource_sync,omitempty"`
}

// RegisterRoutes registers workspace related routes.
func (h *WorkspaceHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/workspaces", h.ListWorkspaces)
	apiV1.POST("/workspaces/sync", h.SyncWorkspaces)
	apiV1.GET("/workspaces/:id", h.GetWorkspace)
}

// workspaceRow represents a workspace in the database.
type workspaceRow struct {
	ID           int64          `db:"id"`
	Name         string         `db:"name"`
	Description  sql.NullString `db:"description"`
	Source       string         `db:"source"`
	Admins       string         `db:"admins"`
	Members      string         `db:"members"`
	LastSyncedAt sql.NullTime   `db:"last_synced_at"`
	CreatedAt    sql.NullTime   `db:"created_at"`
	UpdatedAt    sql.NullTime   `db:"updated_at"`
}

// ListWorkspaces handles workspace listing requests.
//
// @Summary      List workspaces
// @Description  Lists locally cached workspaces with pagination. Keystone always ensures id=0 default workspace exists.
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspace_id query string false "Filter by workspace ID(s), comma-separated; 0 selects default workspace"
// @Param        id           query string false "Alias of workspace_id"
// @Param        keyword      query string false "Search by name"
// @Param        q            query string false "Alias of keyword"
// @Param        search       query string false "Alias of keyword"
// @Param        limit        query int    false "Max results (default 50, max 100)"
// @Param        offset       query int    false "Pagination offset (default 0)"
// @Success      200 {object} WorkspaceListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /workspaces [get]
func (h *WorkspaceHandler) ListWorkspaces(c *gin.Context) {
	if !h.ensureDefaultWorkspace(c) {
		return
	}

	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	workspaceIDs, err := parseNonNegativeInt64List(firstNonEmptyQuery(c, "workspace_id", "id"), "workspace_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	keyword := firstNonEmptyQuery(c, "keyword", "q", "search")
	whereClause := "WHERE w.deleted_at IS NULL"
	args := []any{}
	whereClause, args = appendInt64InFilter(whereClause, args, "w.id", workspaceIDs)
	whereClause, args = appendKeywordSearch(whereClause, args, keyword, "w.name")

	countQuery := "SELECT COUNT(*) FROM workspaces w " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[WORKSPACE] Failed to count workspaces: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspaces"})
		return
	}

	orderClause, orderArgs := keywordOrderBy(keyword, "w.id ASC", "w.name")
	query := `
		SELECT
			w.id,
			w.name,
			w.description,
			w.source,
			w.admins,
			w.members,
			w.last_synced_at,
			w.created_at,
			w.updated_at
		FROM workspaces w
		` + whereClause + `
		` + orderClause + `
		LIMIT ? OFFSET ?
	`
	args = append(args, orderArgs...)
	args = append(args, pagination.Limit, pagination.Offset)

	var dbRows []workspaceRow
	if err := h.db.Select(&dbRows, query, args...); err != nil {
		logger.Printf("[WORKSPACE] Failed to query workspaces: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workspaces"})
		return
	}

	workspaces := []WorkspaceResponse{}
	for _, workspace := range dbRows {
		workspaces = append(workspaces, workspaceResponseFromRow(workspace))
	}

	c.JSON(http.StatusOK, WorkspaceListResponse{
		Items:   workspaces,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: (pagination.Offset + pagination.Limit) < total,
		HasPrev: pagination.Offset > 0,
	})
}

// GetWorkspace handles getting a single workspace by ID.
//
// @Summary      Get workspace
// @Description  Gets a workspace by ID. ID 0 is Keystone's local-only default workspace.
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        id path string true "Workspace ID"
// @Success      200 {object} WorkspaceResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /workspaces/{id} [get]
func (h *WorkspaceHandler) GetWorkspace(c *gin.Context) {
	if !h.ensureDefaultWorkspace(c) {
		return
	}

	id, ok := parseWorkspaceID(c)
	if !ok {
		return
	}

	var workspace workspaceRow
	if err := h.db.Get(&workspace, `
		SELECT id, name, description, source, admins, members, last_synced_at, created_at, updated_at
		FROM workspaces
		WHERE id = ? AND deleted_at IS NULL
	`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		logger.Printf("[WORKSPACE] Failed to query workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get workspace"})
		return
	}

	c.JSON(http.StatusOK, workspaceResponseFromRow(workspace))
}

// SyncWorkspaces handles manual Hilbert workspace sync requests.
//
// @Summary      Sync workspaces from Hilbert
// @Description  Logs in with Keystone's Hilbert service identity and transactionally upserts available Hilbert workspaces.
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Success      200 {object} WorkspaceSyncResponse
// @Failure      503 {object} map[string]string
// @Router       /workspaces/sync [post]
func (h *WorkspaceHandler) SyncWorkspaces(c *gin.Context) {
	if h.syncService == nil || !h.syncService.Configured() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace sync is not configured"})
		return
	}

	result, err := h.syncService.Sync(c.Request.Context())
	if err != nil {
		if errors.Is(err, services.ErrWorkspaceSyncNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace sync is not configured"})
			return
		}
		logger.Printf("[WORKSPACE] Hilbert workspace sync failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace sync failed"})
		return
	}

	c.JSON(http.StatusOK, WorkspaceSyncResponse{
		SyncedCount:     result.SyncedCount,
		DefaultIncluded: result.DefaultIncluded,
		LastSyncedAt:    result.LastSyncedAt.UTC().Format(time.RFC3339),
		ResourceSync:    result.ResourceSync,
	})
}

func (h *WorkspaceHandler) ensureDefaultWorkspace(c *gin.Context) bool {
	var activeDefaultExists bool
	if err := h.db.Get(&activeDefaultExists, "SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = ? AND deleted_at IS NULL)", defaultWorkspaceID); err != nil {
		logger.Printf("[WORKSPACE] Failed to check default workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to ensure default workspace"})
		return false
	}
	if activeDefaultExists {
		return true
	}

	now := time.Now().UTC()
	result, err := h.db.Exec(`
		UPDATE workspaces
		SET
			name = ?,
			description = ?,
			source = ?,
			admins = ?,
			members = ?,
			deleted_at = NULL,
			updated_at = ?
		WHERE id = ?
	`,
		defaultWorkspaceName,
		defaultWorkspaceDescription,
		workspaceSourceDefault,
		"[]",
		"[]",
		now,
		defaultWorkspaceID,
	)
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to update default workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to ensure default workspace"})
		return false
	}
	affected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to inspect default workspace update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to ensure default workspace"})
		return false
	}
	if affected > 0 {
		return true
	}

	if _, err := h.db.Exec(`
		INSERT INTO workspaces (
			id,
			name,
			description,
			source,
			admins,
			members,
			last_synced_at,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		defaultWorkspaceID,
		defaultWorkspaceName,
		defaultWorkspaceDescription,
		workspaceSourceDefault,
		"[]",
		"[]",
		sql.NullTime{},
		now,
		now,
	); err != nil {
		logger.Printf("[WORKSPACE] Failed to insert default workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to ensure default workspace"})
		return false
	}
	return true
}

func parseWorkspaceID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return 0, false
	}
	return id, true
}

func parseNonNegativeInt64List(raw string, fieldName string) ([]int64, error) {
	items, err := splitBoundedMultiValueItems([]string{raw}, fieldName, maxMultiValueFilterIntegerItemLength)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	seen := make(map[int64]struct{})
	values := []int64{}
	for _, item := range items {
		parsed, err := strconv.ParseInt(item, 10, 64)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid %s format", fieldName)
		}
		if _, ok := seen[parsed]; ok {
			continue
		}
		seen[parsed] = struct{}{}
		values = append(values, parsed)
	}
	return values, nil
}

func formatWorkspaceNullableTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339)
}

// workspaceResponseFromRow converts a workspaceRow to a WorkspaceResponse.
func workspaceResponseFromRow(workspace workspaceRow) WorkspaceResponse {
	admins, _ := services.DecodeWorkspacePeople(workspace.Admins)
	members, _ := services.DecodeWorkspacePeople(workspace.Members)
	return WorkspaceResponse{
		ID:           strconv.FormatInt(workspace.ID, 10),
		Name:         workspace.Name,
		Description:  workspace.Description.String,
		Source:       workspace.Source,
		Admins:       admins,
		Members:      members,
		LastSyncedAt: formatWorkspaceNullableTime(workspace.LastSyncedAt),
		CreatedAt:    formatWorkspaceNullableTime(workspace.CreatedAt),
		UpdatedAt:    formatWorkspaceNullableTime(workspace.UpdatedAt),
	}
}
