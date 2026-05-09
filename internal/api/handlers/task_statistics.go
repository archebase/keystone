// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/middleware"
)

type taskBreakdownQuery struct {
	Dimension     string
	WorkstationID string
	Status        string
	StartTime     *time.Time
	EndTime       *time.Time
}

type taskBreakdownResponse struct {
	Dimension string              `json:"dimension"`
	Items     []taskBreakdownItem `json:"items"`
	Total     int64               `json:"total"`
	Limit     int                 `json:"limit"`
	Offset    int                 `json:"offset"`
	HasNext   bool                `json:"hasNext,omitempty"`
	HasPrev   bool                `json:"hasPrev,omitempty"`
}

type taskBreakdownItem struct {
	ID    string `json:"id" db:"id"`
	Name  string `json:"name" db:"name"`
	Count int64  `json:"count" db:"task_count"`
}

// GetTaskBreakdown returns paginated task counts grouped by scene or SOP.
func (h *TaskHandler) GetTaskBreakdown(c *gin.Context) {
	claims := middleware.GetClaims(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	q, err := parseTaskBreakdownQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	idExpr, nameExpr, err := taskBreakdownExpressions(q.Dimension)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	whereClause, args := taskBreakdownWhereClause(q)
	if claims.Role == "data_collector" {
		whereClause += `
			AND EXISTS (
				SELECT 1
				FROM workstations ws_scope
				WHERE ws_scope.id = tasks.workstation_id
					AND ws_scope.data_collector_id = ?
					AND ws_scope.deleted_at IS NULL
			)`
		args = append(args, claims.CollectorID)
	} else if claims.Role == "admin" {
		if q.WorkstationID != "" {
			whereClause += " AND CAST(tasks.workstation_id AS CHAR) = ?"
			args = append(args, q.WorkstationID)
		}
	} else {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
		return
	}

	baseSQL := taskBreakdownBaseSQL(whereClause)
	countQuery := taskBreakdownCountSQL(idExpr, baseSQL)
	var total int64
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[TASK] breakdown count query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get task breakdown"})
		return
	}

	query := taskBreakdownSQL(idExpr, nameExpr, baseSQL)
	queryArgs := append(append([]interface{}{}, args...), pagination.Limit, pagination.Offset)
	items := make([]taskBreakdownItem, 0)
	if err := h.db.Select(&items, query, queryArgs...); err != nil {
		logger.Printf("[TASK] breakdown query failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get task breakdown"})
		return
	}

	c.JSON(http.StatusOK, taskBreakdownResponse{
		Dimension: q.Dimension,
		Items:     items,
		Total:     total,
		Limit:     pagination.Limit,
		Offset:    pagination.Offset,
		HasNext:   int64(pagination.Offset+pagination.Limit) < total,
		HasPrev:   pagination.Offset > 0,
	})
}

func parseTaskBreakdownQuery(c *gin.Context) (taskBreakdownQuery, error) {
	q := taskBreakdownQuery{
		Dimension:     strings.TrimSpace(c.DefaultQuery("dimension", "scene")),
		WorkstationID: strings.TrimSpace(c.Query("workstation_id")),
		Status:        strings.TrimSpace(c.Query("status")),
	}
	if q.Status != "" {
		if _, ok := validTaskStatuses[q.Status]; !ok {
			return taskBreakdownQuery{}, fmt.Errorf("invalid status")
		}
	}

	startTime, err := parseOptionalTaskStatsTime(c.Query("start_time"))
	if err != nil {
		return taskBreakdownQuery{}, fmt.Errorf("start_time must be RFC3339")
	}
	endTime, err := parseOptionalTaskStatsTime(c.Query("end_time"))
	if err != nil {
		return taskBreakdownQuery{}, fmt.Errorf("end_time must be RFC3339")
	}
	if startTime != nil && endTime != nil && !endTime.After(*startTime) {
		return taskBreakdownQuery{}, fmt.Errorf("end_time must be after start_time")
	}
	q.StartTime = startTime
	q.EndTime = endTime

	return q, nil
}

func parseOptionalTaskStatsTime(raw string) (*time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			utc := t.UTC()
			return &utc, nil
		}
	}
	return nil, fmt.Errorf("invalid timestamp")
}

func taskBreakdownExpressions(dimension string) (string, string, error) {
	switch dimension {
	case "scene":
		return "COALESCE(CAST(tasks.scene_id AS CHAR), NULLIF(TRIM(tasks.scene_name), ''), '')",
			`COALESCE(
				NULLIF(TRIM(tasks.scene_name), ''),
				CASE
					WHEN tasks.scene_id IS NULL THEN '未分类'
					ELSE CONCAT('场景 #', CAST(tasks.scene_id AS CHAR))
				END
			)`, nil
	case "sop":
		return "COALESCE(CAST(tasks.sop_id AS CHAR), '')",
			`CASE
				WHEN tasks.sop_id IS NULL THEN '未分类'
				WHEN NULLIF(s.slug, '') IS NULL THEN CONCAT('SOP #', CAST(tasks.sop_id AS CHAR))
				WHEN NULLIF(s.version, '') IS NULL THEN s.slug
				ELSE CONCAT(s.slug, ' @ ', s.version)
			END`, nil
	default:
		return "", "", fmt.Errorf("dimension must be one of scene, sop")
	}
}

func taskBreakdownWhereClause(q taskBreakdownQuery) (string, []interface{}) {
	conditions := []string{"tasks.deleted_at IS NULL"}
	args := make([]interface{}, 0, 6)

	if q.Status != "" {
		conditions = append(conditions, "tasks.status = ?")
		args = append(args, q.Status)
	}
	if q.StartTime != nil {
		conditions = append(conditions, "tasks.created_at >= ?")
		args = append(args, *q.StartTime)
	}
	if q.EndTime != nil {
		conditions = append(conditions, "tasks.created_at < ?")
		args = append(args, *q.EndTime)
	}

	return strings.Join(conditions, " AND "), args
}

func taskBreakdownBaseSQL(whereClause string) string {
	return fmt.Sprintf(`
		FROM tasks
		LEFT JOIN sops s ON s.id = tasks.sop_id AND s.deleted_at IS NULL
		WHERE %s
	`, whereClause)
}

func taskBreakdownCountSQL(idExpr string, baseSQL string) string {
	return fmt.Sprintf(`
		SELECT COUNT(1)
		FROM (
			SELECT %s AS id
			%s
			GROUP BY %s
		) grouped
	`, idExpr, baseSQL, idExpr)
}

func taskBreakdownSQL(idExpr string, nameExpr string, baseSQL string) string {
	return fmt.Sprintf(`
		SELECT
			%s AS id,
			COALESCE(NULLIF(MAX(%s), ''), '未分类') AS name,
			COUNT(1) AS task_count
		%s
		GROUP BY %s
		ORDER BY task_count DESC, name ASC
		LIMIT ? OFFSET ?
	`, idExpr, nameExpr, baseSQL, idExpr)
}
