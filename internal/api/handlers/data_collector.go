// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// DataCollectorHandler handles data collector related HTTP requests.
type DataCollectorHandler struct {
	db *sqlx.DB
}

// NewDataCollectorHandler creates a new DataCollectorHandler.
func NewDataCollectorHandler(db *sqlx.DB) *DataCollectorHandler {
	return &DataCollectorHandler{db: db}
}

// DataCollectorResponse represents one global data collector identity.
type DataCollectorResponse struct {
	ID            string      `json:"id"`
	WorkspaceID   string      `json:"workspace_id,omitempty"`
	WorkspaceName string      `json:"workspace_name,omitempty"`
	WorkspaceIDs  []string    `json:"workspace_ids"`
	Name          string      `json:"name"`
	OperatorID    string      `json:"operator_id"`
	Email         string      `json:"email,omitempty"`
	Certification string      `json:"certification,omitempty"`
	Status        string      `json:"status"`
	Metadata      interface{} `json:"metadata,omitempty"`
	CreatedAt     string      `json:"created_at,omitempty"`
	UpdatedAt     string      `json:"updated_at,omitempty"`
}

// DataCollectorListResponse represents the response for listing data collectors.
type DataCollectorListResponse struct {
	Items   []DataCollectorResponse `json:"items"`
	Total   int                     `json:"total"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	HasNext bool                    `json:"hasNext,omitempty"`
	HasPrev bool                    `json:"hasPrev,omitempty"`
}

// CreateDataCollectorRequest represents a local default Workspace membership request.
type CreateDataCollectorRequest struct {
	WorkspaceID   string      `json:"workspace_id"`
	Name          string      `json:"name"`
	OperatorID    string      `json:"operator_id"`
	Email         string      `json:"email,omitempty"`
	Certification string      `json:"certification,omitempty"`
	Password      string      `json:"password,omitempty"` // #nosec G117 -- Keystone ignores collector passwords
	Metadata      interface{} `json:"metadata,omitempty"`
}

// CreateDataCollectorResponse represents a created or reused global collector.
type CreateDataCollectorResponse = DataCollectorResponse

// UpdateDataCollectorRequest represents global identity fields that can be updated.
type UpdateDataCollectorRequest struct {
	Name          *string         `json:"name,omitempty"`
	OperatorID    *string         `json:"operator_id,omitempty"`
	Email         *string         `json:"email,omitempty"`
	Certification *string         `json:"certification,omitempty"`
	Status        *string         `json:"status,omitempty"`
	Password      *string         `json:"password,omitempty"` // #nosec G117 -- Keystone ignores collector passwords
	Metadata      json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

type dataCollectorRow struct {
	ID            int64          `db:"id"`
	Name          string         `db:"name"`
	OperatorID    string         `db:"operator_id"`
	Email         sql.NullString `db:"email"`
	Certification sql.NullString `db:"certification"`
	Status        string         `db:"status"`
	Metadata      sql.NullString `db:"metadata"`
	CreatedAt     sql.NullTime   `db:"created_at"`
	UpdatedAt     sql.NullTime   `db:"updated_at"`
}

type collectorWorkspaceContext struct {
	ID      int64
	Name    string
	Enabled bool
}

const dataCollectorSelectColumns = `
	dc.id,
	dc.name,
	dc.operator_id,
	dc.email,
	dc.certification,
	dc.status,
	dc.metadata,
	dc.created_at,
	dc.updated_at
`

// RegisterRoutes registers data collector related routes.
func (h *DataCollectorHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/data_collectors", h.ListDataCollectors)
	apiV1.POST("/data_collectors", h.CreateDataCollector)
	apiV1.GET("/data_collectors/:id", h.GetDataCollector)
	apiV1.PUT("/data_collectors/:id", h.UpdateDataCollector)
	apiV1.DELETE("/data_collectors/:id", h.DeleteDataCollector)
}

// ListDataCollectors lists global identities, optionally scoped by Workspace membership.
//
// @Summary      List data collectors
// @Description  Lists global collector identities, optionally filtered by Workspace admins/members
// @Tags         data_collectors
// @Produce      json
// @Param        workspace_id          query string false "Filter by Workspace ID(s), comma-separated"
// @Param        status                query string false "Filter by status(es), comma-separated"
// @Param        collector_operator_id query string false "Filter by collector operator ID(s), comma-separated"
// @Param        collector_name        query string false "Filter by collector name(s), comma-separated"
// @Param        keyword               query string false "Search by name, operator ID, or email"
// @Param        limit                 query int false "Max results (default 50, max 100)"
// @Param        offset                query int false "Pagination offset (default 0)"
// @Success      200 {object} DataCollectorListResponse
// @Failure      400 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors [get]
func (h *DataCollectorHandler) ListDataCollectors(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}
	workspaceIDs, err := parseNonNegativeInt64List(c.Query("workspace_id"), "workspace_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	statuses, err := parseNonEmptyStringList(c.Query("status"), "status")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	operatorIDs, err := parseNonEmptyStringList(c.Query("collector_operator_id"), "collector_operator_id")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	names, err := parseNonEmptyStringList(firstNonEmptyQuery(c, "collector_name", "name"), "collector_name")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	whereClause := "WHERE dc.deleted_at IS NULL"
	args := []any{}
	if len(workspaceIDs) > 0 {
		workspaceOperators, accessErr := services.WorkspaceOperatorIDs(c.Request.Context(), h.db, workspaceIDs)
		if accessErr != nil {
			logger.Printf("[DC] Failed to resolve Workspace collectors: %v", accessErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
			return
		}
		if len(workspaceOperators) == 0 {
			c.JSON(http.StatusOK, emptyDataCollectorList(pagination))
			return
		}
		whereClause, args = appendStringInFilter(whereClause, args, "dc.operator_id", workspaceOperators)
	}
	whereClause, args = appendStringInFilter(whereClause, args, "dc.status", statuses)
	whereClause, args = appendStringInFilter(whereClause, args, "dc.operator_id", operatorIDs)
	whereClause, args = appendStringInFilter(whereClause, args, "dc.name", names)
	keyword := firstNonEmptyQuery(c, "keyword", "q", "search", "operator_id")
	if keyword != "" {
		likeKeyword := "%" + keyword + "%"
		whereClause += " AND (dc.name LIKE ? OR dc.operator_id LIKE ? OR dc.email LIKE ?)"
		args = append(args, likeKeyword, likeKeyword, likeKeyword)
	}

	var total int
	if err := h.db.Get(&total, "SELECT COUNT(*) FROM data_collectors dc "+whereClause, args...); err != nil {
		logger.Printf("[DC] Failed to count data collectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}
	orderClause, orderArgs := keywordOrderBy(keyword, "dc.id DESC", "dc.operator_id", "dc.name", "dc.email")
	query := "SELECT " + dataCollectorSelectColumns + " FROM data_collectors dc " + whereClause + " " + orderClause + " LIMIT ? OFFSET ?"
	queryArgs := append(args, orderArgs...)
	queryArgs = append(queryArgs, pagination.Limit, pagination.Offset)
	rows := []dataCollectorRow{}
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[DC] Failed to query data collectors: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}

	workspaceContext, err := h.singleWorkspaceContext(c.Request.Context(), workspaceIDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
		return
	}
	items := make([]DataCollectorResponse, 0, len(rows))
	for _, row := range rows {
		response, responseErr := h.dataCollectorResponse(c.Request.Context(), row, workspaceContext)
		if responseErr != nil {
			logger.Printf("[DC] Failed to resolve collector Workspaces: %v", responseErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list data collectors"})
			return
		}
		items = append(items, response)
	}
	c.JSON(http.StatusOK, DataCollectorListResponse{
		Items: items, Total: total, Limit: pagination.Limit, Offset: pagination.Offset,
		HasNext: pagination.Offset+pagination.Limit < total, HasPrev: pagination.Offset > 0,
	})
}

func emptyDataCollectorList(pagination PaginationParams) DataCollectorListResponse {
	return DataCollectorListResponse{Items: []DataCollectorResponse{}, Limit: pagination.Limit, Offset: pagination.Offset}
}

// CreateDataCollector creates or reuses a global identity and adds it to the default Workspace.
//
// @Summary      Add a data collector to a local Workspace
// @Description  Creates or reuses a global collector identity and appends its operator code to a default Workspace; Hilbert Workspace membership is read-only
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        body body CreateDataCollectorRequest true "Collector identity and local Workspace membership"
// @Success      201 {object} CreateDataCollectorResponse
// @Failure      400 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors [post]
func (h *DataCollectorHandler) CreateDataCollector(c *gin.Context) {
	var req CreateDataCollectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	req.Name = strings.TrimSpace(req.Name)
	req.OperatorID = strings.TrimSpace(req.OperatorID)
	req.Email = strings.TrimSpace(req.Email)
	req.Certification = strings.TrimSpace(req.Certification)
	workspaceID, err := strconv.ParseInt(req.WorkspaceID, 10, 64)
	if req.WorkspaceID == "" || err != nil || workspaceID < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace_id format"})
		return
	}
	if req.Name == "" || req.OperatorID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and operator_id are required"})
		return
	}

	tx, err := h.db.BeginTxx(c.Request.Context(), nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	var workspace struct {
		Name    string `db:"name"`
		Source  string `db:"source"`
		Members string `db:"members"`
	}
	if err := tx.Get(&workspace, `SELECT name, source, members FROM workspaces WHERE id = ? AND deleted_at IS NULL`, workspaceID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusBadRequest, gin.H{"error": "workspace not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	if workspace.Source != workspaceSourceDefault {
		c.JSON(http.StatusConflict, gin.H{"error": "hilbert workspace membership is read-only"})
		return
	}

	collectorID, err := upsertLocalDataCollector(c.Request.Context(), tx, req)
	if err != nil {
		logger.Printf("[DC] Failed to upsert data collector: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	members, err := services.DecodeWorkspacePeople(workspace.Members)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	members = append(members, req.OperatorID)
	encodedMembers, err := services.EncodeWorkspacePeople(members)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	if _, err := tx.Exec(`UPDATE workspaces SET members = ?, updated_at = ? WHERE id = ?`, encodedMembers, time.Now().UTC(), workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	row, err := h.getDataCollectorRow(collectorID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	response, err := h.dataCollectorResponse(c.Request.Context(), row, collectorWorkspaceContext{ID: workspaceID, Name: workspace.Name, Enabled: true})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create data collector"})
		return
	}
	c.JSON(http.StatusCreated, response)
}

func upsertLocalDataCollector(ctx context.Context, tx *sqlx.Tx, req CreateDataCollectorRequest) (int64, error) {
	var existingID int64
	err := tx.GetContext(ctx, &existingID, `SELECT id FROM data_collectors WHERE operator_id = ? AND deleted_at IS NULL`, req.OperatorID)
	if err == nil {
		return existingID, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	metadata := "{}"
	if req.Metadata != nil {
		encoded, marshalErr := json.Marshal(req.Metadata)
		if marshalErr != nil {
			return 0, marshalErr
		}
		metadata = string(encoded)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO data_collectors (
			name, operator_id, email, password_hash, certification, status, metadata, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, req.Name, req.OperatorID, dcNullableString(req.Email), sql.NullString{}, dcNullableString(req.Certification), "active", metadata, time.Now().UTC(), time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetDataCollector gets one global collector identity.
//
// @Summary      Get data collector
// @Description  Gets one global collector identity and its accessible Workspace IDs
// @Tags         data_collectors
// @Produce      json
// @Param        id path string true "Data Collector ID"
// @Success      200 {object} DataCollectorResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors/{id} [get]
func (h *DataCollectorHandler) GetDataCollector(c *gin.Context) {
	id, ok := parseDataCollectorID(c)
	if !ok {
		return
	}
	row, err := h.getDataCollectorRow(id)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data collector"})
		return
	}
	response, err := h.dataCollectorResponse(c.Request.Context(), row, collectorWorkspaceContext{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get data collector"})
		return
	}
	c.JSON(http.StatusOK, response)
}

// UpdateDataCollector updates global identity fields. operator_id is immutable.
//
// @Summary      Update data collector
// @Description  Updates global collector identity fields; operator_id and Workspace membership are immutable here
// @Tags         data_collectors
// @Accept       json
// @Produce      json
// @Param        id path string true "Data Collector ID"
// @Param        body body UpdateDataCollectorRequest true "Global collector identity fields"
// @Success      200 {object} DataCollectorResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors/{id} [put]
func (h *DataCollectorHandler) UpdateDataCollector(c *gin.Context) {
	id, ok := parseDataCollectorID(c)
	if !ok {
		return
	}
	var req UpdateDataCollectorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.OperatorID != nil {
		var current string
		if err := h.db.Get(&current, `SELECT operator_id FROM data_collectors WHERE id = ? AND deleted_at IS NULL`, id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
			return
		}
		if strings.TrimSpace(*req.OperatorID) != current {
			c.JSON(http.StatusBadRequest, gin.H{"error": "operator_id is immutable"})
			return
		}
	}
	updates, args, err := dataCollectorUpdates(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}
	updates = append(updates, "updated_at = ?")
	args = append(args, time.Now().UTC(), id)
	query := fmt.Sprintf("UPDATE data_collectors SET %s WHERE id = ? AND deleted_at IS NULL", strings.Join(updates, ", ")) //nolint:gosec // columns are fixed literals
	result, err := h.db.Exec(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update data collector"})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		_, _ = h.db.Exec(`UPDATE workstations SET collector_name = ? WHERE data_collector_id = ? AND deleted_at IS NULL`, strings.TrimSpace(*req.Name), id)
	}
	row, err := h.getDataCollectorRow(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated data collector"})
		return
	}
	response, err := h.dataCollectorResponse(c.Request.Context(), row, collectorWorkspaceContext{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get updated data collector"})
		return
	}
	c.JSON(http.StatusOK, response)
}

func dataCollectorUpdates(req UpdateDataCollectorRequest) ([]string, []any, error) {
	updates := []string{}
	args := []any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("name cannot be empty")
		}
		updates = append(updates, "name = ?")
		args = append(args, name)
	}
	if req.Email != nil {
		updates = append(updates, "email = ?")
		args = append(args, dcNullableString(strings.TrimSpace(*req.Email)))
	}
	if req.Certification != nil {
		updates = append(updates, "certification = ?")
		args = append(args, dcNullableString(strings.TrimSpace(*req.Certification)))
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if status != "active" && status != "inactive" && status != "on_leave" {
			return nil, nil, fmt.Errorf("invalid status, must be one of: active, inactive, on_leave")
		}
		updates = append(updates, "status = ?")
		args = append(args, status)
	}
	if len(req.Metadata) > 0 {
		metadata := bytes.TrimSpace(req.Metadata)
		if bytes.Equal(metadata, []byte("null")) {
			metadata = []byte("{}")
		} else {
			var probe any
			if err := json.Unmarshal(metadata, &probe); err != nil {
				return nil, nil, fmt.Errorf("invalid metadata JSON")
			}
		}
		updates = append(updates, "metadata = ?")
		args = append(args, string(metadata))
	}
	return updates, args, nil
}

// DeleteDataCollector removes local membership or soft deletes an unused global identity.
//
// @Summary      Remove data collector membership or identity
// @Description  With workspace_id, removes membership from a default Workspace; otherwise soft deletes an unused global identity
// @Tags         data_collectors
// @Param        id path string true "Data Collector ID"
// @Param        workspace_id query string false "Remove membership from this default Workspace instead of deleting the global identity"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      409 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /data_collectors/{id} [delete]
func (h *DataCollectorHandler) DeleteDataCollector(c *gin.Context) {
	id, ok := parseDataCollectorID(c)
	if !ok {
		return
	}
	workspaceIDRaw := strings.TrimSpace(c.Query("workspace_id"))
	if workspaceIDRaw != "" {
		workspaceID, err := strconv.ParseInt(workspaceIDRaw, 10, 64)
		if err != nil || workspaceID < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace_id format"})
			return
		}
		if err := h.removeLocalWorkspaceMembership(c.Request.Context(), id, workspaceID); err != nil {
			switch err {
			case sql.ErrNoRows:
				c.JSON(http.StatusNotFound, gin.H{"error": "data collector or workspace not found"})
			case errHilbertWorkspaceReadOnly:
				c.JSON(http.StatusConflict, gin.H{"error": "hilbert workspace membership is read-only"})
			default:
				logger.Printf("[DC] Failed to remove local Workspace membership: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove data collector membership"})
			}
			return
		}
		c.Status(http.StatusNoContent)
		return
	}
	var used bool
	if err := h.db.Get(&used, `SELECT EXISTS(SELECT 1 FROM workstations WHERE data_collector_id = ? AND deleted_at IS NULL)`, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}
	if used {
		c.JSON(http.StatusConflict, gin.H{"error": "data collector is assigned to one or more workstations"})
		return
	}
	now := time.Now().UTC()
	result, err := h.db.Exec(`UPDATE data_collectors SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete data collector"})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "data collector not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

var errHilbertWorkspaceReadOnly = fmt.Errorf("hilbert workspace membership is read-only")

func (h *DataCollectorHandler) removeLocalWorkspaceMembership(ctx context.Context, collectorID, workspaceID int64) error {
	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var operatorID string
	if err := tx.GetContext(ctx, &operatorID, `SELECT operator_id FROM data_collectors WHERE id = ? AND deleted_at IS NULL`, collectorID); err != nil {
		return err
	}
	var workspace struct {
		Source  string `db:"source"`
		Members string `db:"members"`
	}
	if err := tx.GetContext(ctx, &workspace, `SELECT source, members FROM workspaces WHERE id = ? AND deleted_at IS NULL`, workspaceID); err != nil {
		return err
	}
	if workspace.Source != workspaceSourceDefault {
		return errHilbertWorkspaceReadOnly
	}
	members, err := services.DecodeWorkspacePeople(workspace.Members)
	if err != nil {
		return err
	}
	filtered := members[:0]
	for _, member := range members {
		if member != operatorID {
			filtered = append(filtered, member)
		}
	}
	encoded, err := services.EncodeWorkspacePeople(filtered)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET members = ?, updated_at = ? WHERE id = ?`, encoded, time.Now().UTC(), workspaceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *DataCollectorHandler) getDataCollectorRow(id int64) (dataCollectorRow, error) {
	var row dataCollectorRow
	err := h.db.Get(&row, "SELECT "+dataCollectorSelectColumns+" FROM data_collectors dc WHERE dc.id = ? AND dc.deleted_at IS NULL", id)
	return row, err
}

func (h *DataCollectorHandler) dataCollectorResponse(ctx context.Context, row dataCollectorRow, workspace collectorWorkspaceContext) (DataCollectorResponse, error) {
	workspaceIDs, err := services.AccessibleWorkspaceIDs(ctx, h.db, row.OperatorID)
	if err != nil {
		return DataCollectorResponse{}, err
	}
	formattedIDs := make([]string, 0, len(workspaceIDs))
	for _, id := range workspaceIDs {
		formattedIDs = append(formattedIDs, strconv.FormatInt(id, 10))
	}
	response := DataCollectorResponse{
		ID: fmt.Sprintf("%d", row.ID), WorkspaceIDs: formattedIDs, Name: row.Name, OperatorID: row.OperatorID,
		Status: row.Status, Metadata: dcMetadataFromDB(row.Metadata),
	}
	if workspace.Enabled {
		response.WorkspaceID = strconv.FormatInt(workspace.ID, 10)
		response.WorkspaceName = workspace.Name
	}
	if row.Email.Valid {
		response.Email = row.Email.String
	}
	if row.Certification.Valid {
		response.Certification = row.Certification.String
	}
	if row.CreatedAt.Valid {
		response.CreatedAt = row.CreatedAt.Time.UTC().Format(time.RFC3339)
	}
	if row.UpdatedAt.Valid {
		response.UpdatedAt = row.UpdatedAt.Time.UTC().Format(time.RFC3339)
	}
	return response, nil
}

func (h *DataCollectorHandler) singleWorkspaceContext(ctx context.Context, workspaceIDs []int64) (collectorWorkspaceContext, error) {
	if len(workspaceIDs) != 1 {
		return collectorWorkspaceContext{}, nil
	}
	var name string
	if err := h.db.GetContext(ctx, &name, `SELECT name FROM workspaces WHERE id = ? AND deleted_at IS NULL`, workspaceIDs[0]); err != nil {
		if err == sql.ErrNoRows {
			return collectorWorkspaceContext{}, nil
		}
		return collectorWorkspaceContext{}, err
	}
	return collectorWorkspaceContext{ID: workspaceIDs[0], Name: name, Enabled: true}, nil
}

func parseDataCollectorID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid data collector id"})
		return 0, false
	}
	return id, true
}

func dcNullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func dcMetadataFromDB(value sql.NullString) interface{} {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	return parseJSONRaw(value.String)
}
