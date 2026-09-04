// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"net/http"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"github.com/gin-gonic/gin"
)

// RobotDeviceNameOption represents one distinct robot/device name option.
type RobotDeviceNameOption struct {
	DeviceID   string `db:"device_id" json:"device_id"`
	DeviceName string `db:"device_name" json:"device_name"`
}

// RobotDeviceNameOptionListResponse represents a paginated list of device name options.
type RobotDeviceNameOptionListResponse struct {
	Items   []RobotDeviceNameOption `json:"items"`
	Total   int                     `json:"total"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	HasNext bool                    `json:"hasNext,omitempty"`
	HasPrev bool                    `json:"hasPrev,omitempty"`
}

// RobotDeviceTypeOption represents one distinct robot device type option.
type RobotDeviceTypeOption struct {
	DeviceType string `db:"device_type" json:"device_type"`
}

// RobotDeviceTypeOptionListResponse represents a paginated list of device type options.
type RobotDeviceTypeOptionListResponse struct {
	Items   []RobotDeviceTypeOption `json:"items"`
	Total   int                     `json:"total"`
	Limit   int                     `json:"limit"`
	Offset  int                     `json:"offset"`
	HasNext bool                    `json:"hasNext,omitempty"`
	HasPrev bool                    `json:"hasPrev,omitempty"`
}

// ListRobotDeviceNameOptions returns distinct projected device names keyed by device_id.
func (h *RobotHandler) ListRobotDeviceNameOptions(c *gin.Context) {
	startedAt := time.Now()
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
	keyword := strings.TrimSpace(c.Query("keyword"))
	whereClause := "WHERE r.deleted_at IS NULL"
	args := []any{}
	whereClause, args = appendInt64InFilter(whereClause, args, "r.workspace_id", workspaceIDs)
	if keyword != "" {
		whereClause += " AND (r.device_id LIKE ? OR " + robotDeviceNameSQLExpr(h.db.DriverName()) + " LIKE ?)"
		like := "%" + keyword + "%"
		args = append(args, like, like)
	}

	countQuery := "SELECT COUNT(*) FROM (SELECT r.device_id FROM robots r " + whereClause + " GROUP BY r.device_id) t"
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[ROBOT] Failed to count device name options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot device names"})
		return
	}

	query := `
		SELECT
			r.device_id,
			MAX(COALESCE(NULLIF(r.device_name, ''), NULLIF(` + robotDeviceNameSQLExpr(h.db.DriverName()) + `, ''), r.device_id)) AS device_name
		FROM robots r
		` + whereClause + `
		GROUP BY r.device_id
		ORDER BY device_name, r.device_id
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var rows []RobotDeviceNameOption
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[ROBOT] Failed to query device name options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot device names"})
		return
	}

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		logger.Printf("[ROBOT] Slow device name options query: total=%s items=%d", elapsed, len(rows))
	}

	c.JSON(http.StatusOK, RobotDeviceNameOptionListResponse{
		Items:   rows,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: pagination.Offset+pagination.Limit < total,
		HasPrev: pagination.Offset > 0,
	})
}

// ListRobotDeviceTypeOptions returns distinct device types for remote selects.
func (h *RobotHandler) ListRobotDeviceTypeOptions(c *gin.Context) {
	startedAt := time.Now()
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
	keyword := strings.TrimSpace(c.Query("keyword"))
	whereClause := "WHERE r.deleted_at IS NULL AND COALESCE(r.device_type, '') <> ''"
	args := []any{}
	whereClause, args = appendInt64InFilter(whereClause, args, "r.workspace_id", workspaceIDs)
	if keyword != "" {
		whereClause += " AND r.device_type LIKE ?"
		args = append(args, "%"+keyword+"%")
	}

	countQuery := "SELECT COUNT(*) FROM (SELECT r.device_type FROM robots r " + whereClause + " GROUP BY r.device_type) t"
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[ROBOT] Failed to count device type options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot device types"})
		return
	}

	query := `
		SELECT r.device_type
		FROM robots r
		` + whereClause + `
		GROUP BY r.device_type
		ORDER BY r.device_type
		LIMIT ? OFFSET ?
	`
	args = append(args, pagination.Limit, pagination.Offset)

	var rows []RobotDeviceTypeOption
	if err := h.db.Select(&rows, query, args...); err != nil {
		logger.Printf("[ROBOT] Failed to query device type options: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list robot device types"})
		return
	}

	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		logger.Printf("[ROBOT] Slow device type options query: total=%s items=%d", elapsed, len(rows))
	}

	c.JSON(http.StatusOK, RobotDeviceTypeOptionListResponse{
		Items:   rows,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: pagination.Offset+pagination.Limit < total,
		HasPrev: pagination.Offset > 0,
	})
}
