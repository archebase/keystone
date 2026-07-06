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
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// WorkspaceHandler handles workspace related HTTP requests.
type WorkspaceHandler struct {
	db *sqlx.DB
}

// NewWorkspaceHandler creates a new WorkspaceHandler.
func NewWorkspaceHandler(db *sqlx.DB) *WorkspaceHandler {
	return &WorkspaceHandler{db: db}
}

// WorkspaceResponse represents a workspace in the response.
type WorkspaceResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Admins    []string `json:"admins"`
	Members   []string `json:"members"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
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

// CreateWorkspaceRequest represents the request body for creating a workspace.
type CreateWorkspaceRequest struct {
	Name    string   `json:"name"`
	Admins  []string `json:"admins"`
	Members []string `json:"members,omitempty"`
}

// UpdateWorkspaceRequest represents the request body for updating a workspace.
type UpdateWorkspaceRequest struct {
	Name    string    `json:"name,omitempty"`
	Admins  *[]string `json:"admins,omitempty"`
	Members *[]string `json:"members,omitempty"`
}

// RegisterRoutes registers workspace related routes.
func (h *WorkspaceHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/workspaces", h.ListWorkspaces)
	apiV1.POST("/workspaces", h.CreateWorkspace)
	apiV1.GET("/workspaces/:id", h.GetWorkspace)
	apiV1.PUT("/workspaces/:id", h.UpdateWorkspace)
	apiV1.DELETE("/workspaces/:id", h.DeleteWorkspace)
}

// workspaceRow represents a workspace in the database.
type workspaceRow struct {
	ID         int64          `db:"id"`
	Name       string         `db:"name"`
	AdminsStr  string         `db:"admins_str"`
	MembersStr sql.NullString `db:"members_str"`
	CreatedAt  sql.NullTime   `db:"created_at"`
	UpdatedAt  sql.NullTime   `db:"updated_at"`
}

// ListWorkspaces handles workspace listing requests.
//
// @Summary      List workspaces
// @Description  Lists all workspaces with pagination
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        workspace_id query string false "Filter by workspace ID(s), comma-separated"
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
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	workspaceIDs, err := parsePositiveInt64List(firstNonEmptyQuery(c, "workspace_id", "id"), "workspace_id")
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

	orderClause, orderArgs := keywordOrderBy(keyword, "w.id DESC", "w.name")
	query := `
		SELECT
			w.id,
			w.name,
			w.admins_str,
			w.members_str,
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
// @Description  Gets a workspace by ID
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
	id, ok := parseWorkspaceID(c)
	if !ok {
		return
	}

	var workspace workspaceRow
	if err := h.db.Get(&workspace, `
		SELECT id, name, admins_str, members_str, created_at, updated_at
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

// CreateWorkspace handles workspace creation requests.
//
// @Summary      Create workspace
// @Description  Creates a new workspace. Workspace names must be unique among non-deleted rows.
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        body body CreateWorkspaceRequest true "Workspace payload"
// @Success      201 {object} WorkspaceResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /workspaces [post]
func (h *WorkspaceHandler) CreateWorkspace(c *gin.Context) {
	var req CreateWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	admins, members, ok := normalizeWorkspacePeople(c, req.Admins, req.Members, true)
	if !ok {
		return
	}

	if !h.ensureWorkspaceNameAvailable(c, name, 0) {
		return
	}

	membersStr := nullableHashWrappedString(members)
	now := time.Now().UTC()

	result, err := h.db.Exec(
		`INSERT INTO workspaces (
			name,
			admins_str,
			members_str,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?)`,
		name,
		joinHashWrappedString(admins),
		membersStr,
		now,
		now,
	)
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to insert workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	id, err := result.LastInsertId()
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to fetch inserted id: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workspace"})
		return
	}

	workspace := workspaceRow{
		ID:         id,
		Name:       name,
		AdminsStr:  joinHashWrappedString(admins),
		MembersStr: membersStr,
		CreatedAt:  sql.NullTime{Time: now, Valid: true},
		UpdatedAt:  sql.NullTime{Time: now, Valid: true},
	}
	c.JSON(http.StatusCreated, workspaceResponseFromRow(workspace))
}

// UpdateWorkspace handles workspace update requests.
//
// @Summary      Update workspace
// @Description  Updates an existing workspace. Workspace names must be unique among non-deleted rows.
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        id   path string                 true "Workspace ID"
// @Param        body body UpdateWorkspaceRequest true "Workspace payload"
// @Success      200 {object} WorkspaceResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /workspaces/{id} [put]
func (h *WorkspaceHandler) UpdateWorkspace(c *gin.Context) {
	id, ok := parseWorkspaceID(c)
	if !ok {
		return
	}

	var req UpdateWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	var existing workspaceRow
	if err := h.db.Get(&existing,
		"SELECT id, name, admins_str, members_str, created_at, updated_at FROM workspaces WHERE id = ? AND deleted_at IS NULL",
		id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
		logger.Printf("[WORKSPACE] Failed to query workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workspace"})
		return
	}

	updates := []string{}
	args := []any{}

	name := strings.TrimSpace(req.Name)
	if name != "" {
		if !h.ensureWorkspaceNameAvailable(c, name, id) {
			return
		}
		updates = append(updates, "name = ?")
		args = append(args, name)
	}

	admins := splitHashWrappedString(existing.AdminsStr)
	members := splitHashWrappedString(existing.MembersStr.String)
	if req.Admins != nil {
		admins = normalizeStringList(*req.Admins)
	}
	if req.Members != nil {
		members = normalizeStringList(*req.Members)
	}
	if req.Admins != nil || req.Members != nil {
		if err := validateWorkspacePeople(admins, members, true); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.Admins != nil {
		updates = append(updates, "admins_str = ?")
		args = append(args, joinHashWrappedString(admins))
	}
	if req.Members != nil {
		updates = append(updates, "members_str = ?")
		args = append(args, nullableHashWrappedString(members))
	}

	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	now := time.Now().UTC()
	updates = append(updates, "updated_at = ?")
	args = append(args, now, id)

	query := fmt.Sprintf("UPDATE workspaces SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", "))
	if _, err := h.db.Exec(query, args...); err != nil {
		logger.Printf("[WORKSPACE] Failed to update workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workspace"})
		return
	}

	var workspace workspaceRow
	if err := h.db.Get(&workspace,
		"SELECT id, name, admins_str, members_str, created_at, updated_at FROM workspaces WHERE id = ?",
		id); err != nil {
		logger.Printf("[WORKSPACE] Failed to fetch updated workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated workspace"})
		return
	}

	c.JSON(http.StatusOK, workspaceResponseFromRow(workspace))
}

// DeleteWorkspace handles workspace deletion requests (soft delete).
//
// @Summary      Delete workspace
// @Description  Soft deletes a workspace by ID
// @Tags         workspaces
// @Accept       json
// @Produce      json
// @Param        id path string true "Workspace ID"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /workspaces/{id} [delete]
func (h *WorkspaceHandler) DeleteWorkspace(c *gin.Context) {
	id, ok := parseWorkspaceID(c)
	if !ok {
		return
	}

	now := time.Now().UTC()
	result, err := h.db.Exec("UPDATE workspaces SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL", now, now, id)
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to delete workspace: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workspace"})
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		logger.Printf("[WORKSPACE] Failed to inspect deleted rows: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workspace"})
		return
	}
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	c.Status(http.StatusNoContent)
}

func parseWorkspaceID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace id"})
		return 0, false
	}
	return id, true
}

func (h *WorkspaceHandler) ensureWorkspaceNameAvailable(c *gin.Context, name string, excludeID int64) bool {
	var nameTaken bool
	if err := h.db.Get(&nameTaken,
		"SELECT EXISTS(SELECT 1 FROM workspaces WHERE name = ? AND id != ? AND deleted_at IS NULL)", name, excludeID); err != nil {
		logger.Printf("[WORKSPACE] Failed to check workspace name: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save workspace"})
		return false
	}
	if nameTaken {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace name already exists"})
		return false
	}
	return true
}

func normalizeWorkspacePeople(c *gin.Context, admins []string, members []string, requireAdmins bool) ([]string, []string, bool) {
	normalizedAdmins := normalizeStringList(admins)
	normalizedMembers := normalizeStringList(members)
	if err := validateWorkspacePeople(normalizedAdmins, normalizedMembers, requireAdmins); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, nil, false
	}
	return normalizedAdmins, normalizedMembers, true
}

func normalizeStringList(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		item := strings.TrimSpace(value)
		if item == "" {
			continue
		}
		normalized = append(normalized, item)
	}
	return normalized
}

func validateWorkspacePeople(admins []string, members []string, requireAdmins bool) error {
	if requireAdmins && len(admins) == 0 {
		return fmt.Errorf("admins is required")
	}
	adminSet := map[string]struct{}{}
	for _, admin := range admins {
		if _, exists := adminSet[admin]; exists {
			return fmt.Errorf("admins contains duplicate value %q", admin)
		}
		adminSet[admin] = struct{}{}
	}
	memberSet := map[string]struct{}{}
	for _, member := range members {
		if _, exists := memberSet[member]; exists {
			return fmt.Errorf("members contains duplicate value %q", member)
		}
		if _, isAdmin := adminSet[member]; isAdmin {
			return fmt.Errorf("admins and members cannot overlap")
		}
		memberSet[member] = struct{}{}
	}
	return nil
}

func joinHashWrappedString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return "#" + strings.Join(values, "#") + "#"
}

func splitHashWrappedString(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{}
	}
	value = strings.Trim(value, "#")
	if value == "" {
		return []string{}
	}
	return normalizeStringList(strings.Split(value, "#"))
}

func nullableHashWrappedString(values []string) sql.NullString {
	if len(values) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: joinHashWrappedString(values), Valid: true}
}

// workspaceResponseFromRow converts a workspaceRow to a WorkspaceResponse.
func workspaceResponseFromRow(workspace workspaceRow) WorkspaceResponse {
	createdAt := ""
	if workspace.CreatedAt.Valid {
		createdAt = workspace.CreatedAt.Time.UTC().Format(time.RFC3339)
	}

	updatedAt := ""
	if workspace.UpdatedAt.Valid {
		updatedAt = workspace.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}

	return WorkspaceResponse{
		ID:        fmt.Sprintf("%d", workspace.ID),
		Name:      workspace.Name,
		Admins:    splitHashWrappedString(workspace.AdminsStr),
		Members:   splitHashWrappedString(workspace.MembersStr.String),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}
