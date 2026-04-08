// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/logger"
)

// EpisodeHandler handles episode-related HTTP requests
type EpisodeHandler struct {
	db *sqlx.DB
}

// NewEpisodeHandler creates a new EpisodeHandler
func NewEpisodeHandler(db *sqlx.DB) *EpisodeHandler {
	return &EpisodeHandler{
		db: db,
	}
}

// Episode represents an episode in the database
type episodeRow struct {
	ID                 string          `db:"episode_id"`
	TaskID             int64           `db:"task_id"`
	TaskPublicID       sql.NullString  `db:"task_public_id"`
	McapPath           string          `db:"mcap_path"`
	SidecarPath        string          `db:"sidecar_path"`
	Checksum           sql.NullString  `db:"checksum"`
	QaStatus           string          `db:"qa_status"`
	QaScore            sql.NullFloat64 `db:"qa_score"`
	AutoApproved       bool            `db:"auto_approved"`
	InspectorID        sql.NullString  `db:"inspector_id"`
	InspectionDecision sql.NullString  `db:"inspection_decision"`
	InspectedAt        sql.NullTime    `db:"inspected_at"`
	CloudProcessed     bool            `db:"cloud_processed"`
	CloudSyncedAt      sql.NullTime    `db:"cloud_synced_at"`
	CreatedAt          time.Time       `db:"created_at"`
	LabelsJSON         sql.NullString  `db:"labels"`
}

// Episode represents an episode in the API response
type Episode struct {
	ID                 string   `json:"id"`
	TaskID             int64    `json:"task_id"`
	TaskPublicID       *string  `json:"task_public_id"`
	McapPath           string   `json:"mcap_path"`
	SidecarPath        string   `json:"sidecar_path"`
	Checksum           *string  `json:"checksum"`
	QaStatus           string   `json:"qa_status"`
	QaScore            *float64 `json:"qa_score"`
	AutoApproved       bool     `json:"auto_approved"`
	InspectorID        *string  `json:"inspector_id"`
	InspectionDecision *string  `json:"inspection_decision"`
	InspectedAt        *string  `json:"inspected_at"`
	CloudProcessed     bool     `json:"cloud_processed"`
	CloudSyncedAt      *string  `json:"cloud_synced_at"`
	CreatedAt          string   `json:"created_at"`
	Labels             []string `json:"labels"`
}

// EpisodeListResponse represents the response for listing episodes
type EpisodeListResponse struct {
	Items   []Episode `json:"items"`
	Total   int       `json:"total"`
	Limit   int       `json:"limit"`
	Offset  int       `json:"offset"`
	HasNext bool      `json:"hasNext,omitempty"`
	HasPrev bool      `json:"hasPrev,omitempty"`
}

// RegisterRoutes registers episode-related routes
func (h *EpisodeHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("", h.ListEpisodes)
	apiV1.GET(":id", h.GetEpisode)
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}

	v := value.String
	return &v
}

func nullableFloat64(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}

	v := value.Float64
	return &v
}

func nullableTime(value sql.NullTime) *string {
	if !value.Valid {
		return nil
	}

	v := value.Time.UTC().Format(time.RFC3339)
	return &v
}

// episodeLabelsFromDB parses episodes.labels JSON (string array). Invalid or empty yields empty slice.
func episodeLabelsFromDB(ns sql.NullString) []string {
	if !ns.Valid || strings.TrimSpace(ns.String) == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(ns.String), &out); err != nil {
		return []string{}
	}
	if out == nil {
		return []string{}
	}
	return out
}

// ListEpisodes returns a list of episodes with filtering and pagination
//
// @Summary      List episodes
// @Description  Returns a list of episodes with optional filtering by task_id, qa_status, auto_approved, and cloud_processed
// @Tags         episodes
// @Produce      json
// @Param        task_id          query     string  false  "Filter by task numeric id (or legacy public task_id string)"
// @Param        qa_status        query     string  false  "Filter by QA status"
// @Param        auto_approved    query     bool    false  "Filter by auto-approval status"
// @Param        cloud_processed  query     bool    false  "Filter by cloud processing status"
// @Param        limit            query     int     false  "Max results (default 50)"
// @Param        offset           query     int     false  "Pagination offset (default 0)"
// @Success      200              {object}  EpisodeListResponse
// @Router       /episodes [get]
func (h *EpisodeHandler) ListEpisodes(c *gin.Context) {
	// Parse query parameters
	taskID := c.Query("task_id")
	qaStatus := c.Query("qa_status")
	autoApproved := c.Query("auto_approved")
	cloudProcessed := c.Query("cloud_processed")

	// Parse limit and offset with defaults
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if err != nil || limit < 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		offset = 0
	}

	// Build the base query
	query := `
		SELECT 
			e.episode_id,
			e.task_id as task_id,
			t.task_id as task_public_id,
			e.mcap_path,
			e.sidecar_path,
			COALESCE(e.qa_status, '') as qa_status,
			COALESCE(e.qa_score, 0) as qa_score,
			e.auto_approved,
			e.cloud_processed,
			e.created_at,
			e.labels
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		WHERE e.deleted_at IS NULL
	`

	countQuery := `
		SELECT COUNT(1)
		FROM episodes e
		WHERE e.deleted_at IS NULL
	`

	args := []interface{}{}
	argsCount := []interface{}{}

	// Add filters
	if taskID != "" {
		// Prefer numeric task primary key (tasks.id / episodes.task_id).
		// For backwards compatibility, also accept legacy public task_id (tasks.task_id).
		if parsed, err := strconv.ParseInt(taskID, 10, 64); err == nil {
			query += " AND e.task_id = ?"
			countQuery += " AND e.task_id = ?"
			args = append(args, parsed)
			argsCount = append(argsCount, parsed)
		} else {
			query += " AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = e.task_id AND t.task_id = ? AND t.deleted_at IS NULL)"
			countQuery += " AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = e.task_id AND t.task_id = ? AND t.deleted_at IS NULL)"
			args = append(args, taskID)
			argsCount = append(argsCount, taskID)
		}
	}

	if qaStatus != "" {
		query += " AND e.qa_status = ?"
		countQuery += " AND e.qa_status = ?"
		args = append(args, qaStatus)
		argsCount = append(argsCount, qaStatus)
	}

	if autoApproved != "" {
		approved, err := strconv.ParseBool(autoApproved)
		if err == nil {
			query += " AND e.auto_approved = ?"
			countQuery += " AND e.auto_approved = ?"
			args = append(args, approved)
			argsCount = append(argsCount, approved)
		}
	}

	if cloudProcessed != "" {
		processed, err := strconv.ParseBool(cloudProcessed)
		if err == nil {
			query += " AND e.cloud_processed = ?"
			countQuery += " AND e.cloud_processed = ?"
			args = append(args, processed)
			argsCount = append(argsCount, processed)
		}
	}

	// Get total count
	var total int
	err = h.db.Get(&total, countQuery, argsCount...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count episodes"})
		return
	}

	// Add ordering and pagination
	query += " ORDER BY e.created_at DESC"
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	// Execute query using sqlx.Select
	// #nosec G201 -- Query is constructed with parameterized inputs
	var rows []episodeRow
	err = h.db.Select(&rows, query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episodes"})
		return
	}

	// Convert to API response format
	episodes := make([]Episode, len(rows))
	for i, r := range rows {
		episodes[i] = Episode{
			ID:                 r.ID,
			TaskID:             r.TaskID,
			TaskPublicID:       nullableString(r.TaskPublicID),
			McapPath:           r.McapPath,
			SidecarPath:        r.SidecarPath,
			Checksum:           nullableString(r.Checksum),
			QaStatus:           r.QaStatus,
			QaScore:            nullableFloat64(r.QaScore),
			AutoApproved:       r.AutoApproved,
			InspectorID:        nullableString(r.InspectorID),
			InspectionDecision: nullableString(r.InspectionDecision),
			InspectedAt:        nullableTime(r.InspectedAt),
			CloudProcessed:     r.CloudProcessed,
			CloudSyncedAt:      nullableTime(r.CloudSyncedAt),
			CreatedAt:          r.CreatedAt.UTC().Format(time.RFC3339),
			Labels:             episodeLabelsFromDB(r.LabelsJSON),
		}
	}

	hasNext := (offset + limit) < total
	hasPrev := offset > 0

	// Return response
	c.JSON(http.StatusOK, EpisodeListResponse{
		Items:   episodes,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// GetEpisode returns episode details by episode ID.
//
// @Summary      Get episode details
// @Description  Returns an episode by ID
// @Tags         episodes
// @Produce      json
// @Param        id   path      string  true  "Episode ID"
// @Success      200  {object}  Episode
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /episodes/{id} [get]
func (h *EpisodeHandler) GetEpisode(c *gin.Context) {
	episodeID := c.Param("id")

	var row episodeRow
	query := `
		SELECT
			e.episode_id,
			e.task_id AS task_id,
			t.task_id AS task_public_id,
			e.mcap_path,
			e.sidecar_path,
			e.checksum,
			COALESCE(e.qa_status, '') AS qa_status,
			e.qa_score,
			e.auto_approved,
			CASE WHEN i.inspector_id IS NULL THEN NULL ELSE ins.inspector_id END AS inspector_id,
			CASE WHEN i.decision IS NULL THEN NULL ELSE i.decision END AS inspection_decision,
			i.inspected_at,
			e.cloud_processed,
			e.cloud_synced_at,
			e.created_at,
			e.labels
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN inspections i ON i.episode_id = e.id
		LEFT JOIN inspectors ins ON ins.id = i.inspector_id
		WHERE e.episode_id = ? AND e.deleted_at IS NULL
		LIMIT 1
	`

	err := h.db.Get(&row, query, episodeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		return
	}

	if err != nil {
		logger.Printf("[EPISODE] Failed to query episode: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episode"})
		return
	}

	c.JSON(http.StatusOK, Episode{
		ID:                 row.ID,
		TaskID:             row.TaskID,
		TaskPublicID:       nullableString(row.TaskPublicID),
		McapPath:           row.McapPath,
		SidecarPath:        row.SidecarPath,
		Checksum:           nullableString(row.Checksum),
		QaStatus:           row.QaStatus,
		QaScore:            nullableFloat64(row.QaScore),
		AutoApproved:       row.AutoApproved,
		InspectorID:        nullableString(row.InspectorID),
		InspectionDecision: nullableString(row.InspectionDecision),
		InspectedAt:        nullableTime(row.InspectedAt),
		CloudProcessed:     row.CloudProcessed,
		CloudSyncedAt:      nullableTime(row.CloudSyncedAt),
		CreatedAt:          row.CreatedAt.UTC().Format(time.RFC3339),
		Labels:             episodeLabelsFromDB(row.LabelsJSON),
	})
}
