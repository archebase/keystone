// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
)

const maxRobotTypeConfigTemplateContentBytes = 256 * 1024

var robotTypeConfigTemplateFilenames = []string{"recorder.yaml", "transfer.yaml"}

// RobotTypeConfigTemplateSummary represents one managed config template slot.
type RobotTypeConfigTemplateSummary struct {
	Filename  string  `json:"filename"`
	Exists    bool    `json:"exists"`
	UpdatedAt *string `json:"updated_at"`
}

// RobotTypeConfigTemplateListResponse represents the admin config template list response.
type RobotTypeConfigTemplateListResponse struct {
	Templates []RobotTypeConfigTemplateSummary `json:"templates"`
}

// RobotTypeConfigTemplateResponse represents a stored config template.
type RobotTypeConfigTemplateResponse struct {
	Filename  string `json:"filename"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// RobotTypeConfigTemplateStatusResponse represents a saved template without returning its content.
type RobotTypeConfigTemplateStatusResponse struct {
	Filename  string `json:"filename"`
	Exists    bool   `json:"exists"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// UpsertRobotTypeConfigTemplateRequest is the admin payload for creating/updating a template.
type UpsertRobotTypeConfigTemplateRequest struct {
	Content string `json:"content"`
}

type robotTypeConfigTemplateRow struct {
	ID        int64        `db:"id"`
	Filename  string       `db:"filename"`
	Content   string       `db:"content"`
	CreatedAt sql.NullTime `db:"created_at"`
	UpdatedAt sql.NullTime `db:"updated_at"`
}

type robotTypeConfigTemplateSummaryRow struct {
	Filename  string       `db:"filename"`
	UpdatedAt sql.NullTime `db:"updated_at"`
}

// RegisterConfigTemplatePublicRoutes registers robot type config template routes that do not require auth.
func (h *RobotTypeHandler) RegisterConfigTemplatePublicRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robot_types/:id/configs/:filename", h.GetRobotTypeConfig)
}

// RegisterConfigTemplateAdminRoutes registers admin-only robot type config template routes.
func (h *RobotTypeHandler) RegisterConfigTemplateAdminRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("/robot_types/:id/config_templates", h.ListRobotTypeConfigTemplates)
	apiV1.GET("/robot_types/:id/config_templates/:filename", h.GetRobotTypeConfigTemplate)
	apiV1.PUT("/robot_types/:id/config_templates/:filename", h.UpsertRobotTypeConfigTemplate)
	apiV1.DELETE("/robot_types/:id/config_templates/:filename", h.DeleteRobotTypeConfigTemplate)
}

// GetRobotTypeConfig returns the raw template content for Axon/Synapse consumers.
//
// @Summary      Get robot type config
// @Description  Returns an unrendered robot type config template by robot type and filename.
// @Tags         robot_type_config_templates
// @Produce      plain
// @Param        robot_type_id path string true "Robot Type ID"
// @Param        filename      path string true "Config filename"
// @Success      200 {string} string
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{robot_type_id}/configs/{filename} [get]
func (h *RobotTypeHandler) GetRobotTypeConfig(c *gin.Context) {
	robotTypeID, filename, ok := parseRobotTypeConfigTemplatePath(c)
	if !ok {
		return
	}

	if ok := h.ensureRobotTypeExists(c, robotTypeID); !ok {
		return
	}

	row, err := h.getActiveRobotTypeConfigTemplate(robotTypeID, filename)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "config template not found"})
			return
		}
		logger.Printf("[ROBOT] Failed to query robot type config template: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get config template"})
		return
	}

	c.Data(http.StatusOK, "text/yaml; charset=utf-8", []byte(row.Content))
}

// ListRobotTypeConfigTemplates lists the fixed config template slots for a robot type.
//
// @Summary      List robot type config templates
// @Description  Lists recorder.yaml and transfer.yaml upload status for a robot type.
// @Tags         robot_type_config_templates
// @Accept       json
// @Produce      json
// @Param        robot_type_id path string true "Robot Type ID"
// @Success      200 {object} RobotTypeConfigTemplateListResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{robot_type_id}/config_templates [get]
func (h *RobotTypeHandler) ListRobotTypeConfigTemplates(c *gin.Context) {
	robotTypeID, ok := parseRobotTypeConfigTemplateID(c)
	if !ok {
		return
	}

	if ok := h.ensureRobotTypeExists(c, robotTypeID); !ok {
		return
	}

	rows := []robotTypeConfigTemplateSummaryRow{}
	if err := h.db.Select(&rows, `
		SELECT filename, updated_at
		FROM robot_type_config_templates
		WHERE robot_type_id = ? AND deleted_at IS NULL
	`, robotTypeID); err != nil {
		logger.Printf("[ROBOT] Failed to list robot type config templates: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list config templates"})
		return
	}

	byFilename := make(map[string]robotTypeConfigTemplateSummaryRow, len(rows))
	for _, row := range rows {
		if isAllowedRobotTypeConfigFilename(row.Filename) {
			byFilename[row.Filename] = row
		}
	}

	templates := make([]RobotTypeConfigTemplateSummary, 0, len(robotTypeConfigTemplateFilenames))
	for _, filename := range robotTypeConfigTemplateFilenames {
		summary := RobotTypeConfigTemplateSummary{Filename: filename, Exists: false}
		if row, ok := byFilename[filename]; ok {
			summary.Exists = true
			summary.UpdatedAt = formatRobotTypeConfigTemplateTimePtr(row.UpdatedAt)
		}
		templates = append(templates, summary)
	}

	c.JSON(http.StatusOK, RobotTypeConfigTemplateListResponse{Templates: templates})
}

// GetRobotTypeConfigTemplate returns one stored template as JSON for admin management.
//
// @Summary      Get robot type config template
// @Description  Gets a robot type config template as JSON for admin management.
// @Tags         robot_type_config_templates
// @Accept       json
// @Produce      json
// @Param        robot_type_id path string true "Robot Type ID"
// @Param        filename      path string true "Config filename"
// @Success      200 {object} RobotTypeConfigTemplateResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{robot_type_id}/config_templates/{filename} [get]
func (h *RobotTypeHandler) GetRobotTypeConfigTemplate(c *gin.Context) {
	robotTypeID, filename, ok := parseRobotTypeConfigTemplatePath(c)
	if !ok {
		return
	}

	if ok := h.ensureRobotTypeExists(c, robotTypeID); !ok {
		return
	}

	row, err := h.getActiveRobotTypeConfigTemplate(robotTypeID, filename)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "config template not found"})
			return
		}
		logger.Printf("[ROBOT] Failed to query robot type config template: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get config template"})
		return
	}

	c.JSON(http.StatusOK, robotTypeConfigTemplateRowToResponse(row))
}

// UpsertRobotTypeConfigTemplate creates or replaces the current active config template.
//
// @Summary      Upsert robot type config template
// @Description  Creates or replaces the current active config template for a robot type.
// @Tags         robot_type_config_templates
// @Accept       json
// @Produce      json
// @Param        robot_type_id path string true "Robot Type ID"
// @Param        filename      path string true "Config filename"
// @Param        body          body   UpsertRobotTypeConfigTemplateRequest true "Config template payload"
// @Success      200 {object} RobotTypeConfigTemplateStatusResponse
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{robot_type_id}/config_templates/{filename} [put]
func (h *RobotTypeHandler) UpsertRobotTypeConfigTemplate(c *gin.Context) {
	robotTypeID, filename, ok := parseRobotTypeConfigTemplatePath(c)
	if !ok {
		return
	}

	var req UpsertRobotTypeConfigTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content is required"})
		return
	}
	if len(req.Content) > maxRobotTypeConfigTemplateContentBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content too large"})
		return
	}

	if ok := h.ensureRobotTypeExists(c, robotTypeID); !ok {
		return
	}

	tx, err := h.db.Begin()
	if err != nil {
		logger.Printf("[ROBOT] Failed to begin robot type config template transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config template"})
		return
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()
	var existingID int64
	err = tx.QueryRow(`
		SELECT id
		FROM robot_type_config_templates
		WHERE robot_type_id = ? AND filename = ? AND deleted_at IS NULL
		LIMIT 1
	`, robotTypeID, filename).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		logger.Printf("[ROBOT] Failed to query robot type config template for update: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config template"})
		return
	}

	if err == sql.ErrNoRows {
		_, err = tx.Exec(`
			INSERT INTO robot_type_config_templates (
				robot_type_id,
				filename,
				content,
				created_at,
				updated_at
			) VALUES (?, ?, ?, ?, ?)
		`, robotTypeID, filename, req.Content, now, now)
	} else {
		_, err = tx.Exec(`
			UPDATE robot_type_config_templates
			SET content = ?, updated_at = ?
			WHERE id = ? AND deleted_at IS NULL
		`, req.Content, now, existingID)
	}
	if err != nil {
		logger.Printf("[ROBOT] Failed to save robot type config template: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config template"})
		return
	}

	if err := tx.Commit(); err != nil {
		logger.Printf("[ROBOT] Failed to commit robot type config template transaction: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config template"})
		return
	}

	row, err := h.getActiveRobotTypeConfigTemplate(robotTypeID, filename)
	if err != nil {
		logger.Printf("[ROBOT] Failed to fetch saved robot type config template: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config template"})
		return
	}

	c.JSON(http.StatusOK, robotTypeConfigTemplateRowToStatusResponse(row))
}

// DeleteRobotTypeConfigTemplate soft deletes the current active config template.
//
// @Summary      Delete robot type config template
// @Description  Deletes the current active config template for a robot type.
// @Tags         robot_type_config_templates
// @Accept       json
// @Produce      json
// @Param        robot_type_id path string true "Robot Type ID"
// @Param        filename      path string true "Config filename"
// @Success      204
// @Failure      400 {object} map[string]string
// @Failure      404 {object} map[string]string
// @Failure      500 {object} map[string]string
// @Router       /robot_types/{robot_type_id}/config_templates/{filename} [delete]
func (h *RobotTypeHandler) DeleteRobotTypeConfigTemplate(c *gin.Context) {
	robotTypeID, filename, ok := parseRobotTypeConfigTemplatePath(c)
	if !ok {
		return
	}

	if ok := h.ensureRobotTypeExists(c, robotTypeID); !ok {
		return
	}

	now := time.Now().UTC()
	if _, err := h.db.Exec(`
		UPDATE robot_type_config_templates
		SET deleted_at = ?, updated_at = ?
		WHERE robot_type_id = ? AND filename = ? AND deleted_at IS NULL
	`, now, now, robotTypeID, filename); err != nil {
		logger.Printf("[ROBOT] Failed to delete robot type config template: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete config template"})
		return
	}

	c.Status(http.StatusNoContent)
}

func parseRobotTypeConfigTemplateID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid robot_type_id"})
		return 0, false
	}
	return id, true
}

func parseRobotTypeConfigTemplatePath(c *gin.Context) (int64, string, bool) {
	id, ok := parseRobotTypeConfigTemplateID(c)
	if !ok {
		return 0, "", false
	}

	filename := strings.TrimSpace(c.Param("filename"))
	if !isAllowedRobotTypeConfigFilename(filename) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid config filename"})
		return 0, "", false
	}

	return id, filename, true
}

func isAllowedRobotTypeConfigFilename(filename string) bool {
	for _, allowed := range robotTypeConfigTemplateFilenames {
		if filename == allowed {
			return true
		}
	}
	return false
}

func (h *RobotTypeHandler) ensureRobotTypeExists(c *gin.Context, id int64) bool {
	var exists bool
	if err := h.db.Get(&exists, "SELECT EXISTS(SELECT 1 FROM robot_types WHERE id = ? AND deleted_at IS NULL)", id); err != nil {
		logger.Printf("[ROBOT] Failed to check robot type existence: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check robot_type"})
		return false
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "robot_type not found"})
		return false
	}
	return true
}

func (h *RobotTypeHandler) getActiveRobotTypeConfigTemplate(robotTypeID int64, filename string) (robotTypeConfigTemplateRow, error) {
	var row robotTypeConfigTemplateRow
	err := h.db.Get(&row, `
		SELECT id, filename, content, created_at, updated_at
		FROM robot_type_config_templates
		WHERE robot_type_id = ? AND filename = ? AND deleted_at IS NULL
		LIMIT 1
	`, robotTypeID, filename)
	return row, err
}

func robotTypeConfigTemplateRowToResponse(row robotTypeConfigTemplateRow) RobotTypeConfigTemplateResponse {
	return RobotTypeConfigTemplateResponse{
		Filename:  row.Filename,
		Content:   row.Content,
		CreatedAt: formatRobotTypeConfigTemplateTime(row.CreatedAt),
		UpdatedAt: formatRobotTypeConfigTemplateTime(row.UpdatedAt),
	}
}

func robotTypeConfigTemplateRowToStatusResponse(row robotTypeConfigTemplateRow) RobotTypeConfigTemplateStatusResponse {
	return RobotTypeConfigTemplateStatusResponse{
		Filename:  row.Filename,
		Exists:    true,
		CreatedAt: formatRobotTypeConfigTemplateTime(row.CreatedAt),
		UpdatedAt: formatRobotTypeConfigTemplateTime(row.UpdatedAt),
	}
}

func formatRobotTypeConfigTemplateTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func formatRobotTypeConfigTemplateTimePtr(t sql.NullTime) *string {
	if !t.Valid {
		return nil
	}
	formatted := t.Time.UTC().Format(time.RFC3339)
	return &formatted
}
