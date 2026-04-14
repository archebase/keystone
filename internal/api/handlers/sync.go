// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

// SyncHandler handles cloud sync related HTTP requests.
type SyncHandler struct {
	db         *sqlx.DB
	syncWorker *services.SyncWorker
}

// NewSyncHandler creates a new SyncHandler.
func NewSyncHandler(db *sqlx.DB, syncWorker *services.SyncWorker) *SyncHandler {
	return &SyncHandler{db: db, syncWorker: syncWorker}
}

// RegisterRoutes registers cloud sync related routes.
func (h *SyncHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.POST("/sync/episodes", h.TriggerBatchSync)
	apiV1.POST("/sync/episodes/:id", h.TriggerEpisodeSync)
	apiV1.GET("/sync/episodes", h.ListSyncJobs)
	apiV1.GET("/sync/episodes/:id/status", h.GetSyncStatus)
	apiV1.GET("/sync/config", h.GetSyncConfig)
}

// syncLogRow represents a row from the sync_logs table.
type syncLogRow struct {
	ID               int64          `db:"id"`
	EpisodeID        int64          `db:"episode_id"`
	EpisodePublicID  sql.NullString `db:"episode_public_id"`
	SourceFactoryID  sql.NullString `db:"source_factory_id"`
	SourcePath       sql.NullString `db:"source_path"`
	DestinationPath  sql.NullString `db:"destination_path"`
	Status           string         `db:"status"`
	BytesTransferred sql.NullInt64  `db:"bytes_transferred"`
	DurationSec      sql.NullInt64  `db:"duration_sec"`
	ErrorMessage     sql.NullString `db:"error_message"`
	AttemptCount     int            `db:"attempt_count"`
	NextRetryAt      sql.NullTime   `db:"next_retry_at"`
	StartedAt        sql.NullTime   `db:"started_at"`
	CompletedAt      sql.NullTime   `db:"completed_at"`
}

// SyncJobResponse represents a sync job in the API response.
type SyncJobResponse struct {
	ID               int64   `json:"id"`
	EpisodeID        int64   `json:"episode_id"`
	EpisodePublicID  *string `json:"episode_public_id,omitempty"`
	SourcePath       *string `json:"source_path,omitempty"`
	DestinationPath  *string `json:"destination_path,omitempty"`
	Status           string  `json:"status"`
	BytesTransferred *int64  `json:"bytes_transferred,omitempty"`
	DurationSec      *int64  `json:"duration_sec,omitempty"`
	ErrorMessage     *string `json:"error_message,omitempty"`
	AttemptCount     int     `json:"attempt_count"`
	NextRetryAt      *string `json:"next_retry_at,omitempty"`
	StartedAt        *string `json:"started_at,omitempty"`
	CompletedAt      *string `json:"completed_at,omitempty"`
}

// SyncJobListResponse represents the response for listing sync jobs.
type SyncJobListResponse struct {
	Items   []SyncJobResponse `json:"items"`
	Total   int               `json:"total"`
	Limit   int               `json:"limit"`
	Offset  int               `json:"offset"`
	HasNext bool              `json:"hasNext,omitempty"`
	HasPrev bool              `json:"hasPrev,omitempty"`
}

// TriggerBatchSync triggers sync for all approved but un-synced episodes.
//
// @Summary      Trigger batch cloud sync
// @Description  Enqueues all approved episodes that have not been synced to cloud
// @Tags         sync
// @Produce      json
// @Success      202  {object}  map[string]interface{}
// @Failure      500  {object}  map[string]string
// @Router       /sync/episodes [post]
func (h *SyncHandler) TriggerBatchSync(c *gin.Context) {
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sync worker is not configured"})
		return
	}

	count, err := h.syncWorker.EnqueuePendingEpisodes(c.Request.Context())
	if err != nil {
		logger.Printf("[SYNC] Batch enqueue failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue episodes"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":         "accepted",
		"enqueued_count": count,
		"message":        fmt.Sprintf("%d episodes enqueued for cloud sync", count),
	})
}

// TriggerEpisodeSync triggers sync for a single episode.
//
// @Summary      Trigger single episode cloud sync
// @Description  Enqueues a specific episode for cloud sync by episode numeric ID
// @Tags         sync
// @Produce      json
// @Param        id   path      int  true  "Episode ID"
// @Success      202  {object}  map[string]interface{}
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /sync/episodes/{id} [post]
func (h *SyncHandler) TriggerEpisodeSync(c *gin.Context) {
	if h.syncWorker == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sync worker is not configured"})
		return
	}

	idStr := c.Param("id")
	episodeID, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || episodeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}

	// Verify episode exists and is approved
	var row struct {
		QaStatus    string `db:"qa_status"`
		CloudSynced bool   `db:"cloud_synced"`
	}
	err = h.db.Get(&row, "SELECT qa_status, cloud_synced FROM episodes WHERE id = ? AND deleted_at IS NULL", episodeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		return
	}
	if err != nil {
		logger.Printf("[SYNC] Failed to query episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episode"})
		return
	}

	if row.CloudSynced {
		c.JSON(http.StatusConflict, gin.H{"error": "episode already synced to cloud"})
		return
	}

	if row.QaStatus != "approved" && row.QaStatus != "inspector_approved" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("episode qa_status is %q, must be approved or inspector_approved", row.QaStatus),
		})
		return
	}

	err = h.syncWorker.EnqueueEpisodeManual(c.Request.Context(), episodeID)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrEpisodeAlreadyEnqueued), errors.Is(err, services.ErrSyncAlreadyInProgress):
			c.JSON(http.StatusConflict, gin.H{
				"error":      err.Error(),
				"episode_id": episodeID,
				"status":     "already_queued",
			})
			return
		case errors.Is(err, services.ErrSyncQueueFull):
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":      err.Error(),
				"episode_id": episodeID,
				"status":     "queue_full",
			})
			return
		}
		logger.Printf("[SYNC] Enqueue episode %d failed: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue episode"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":     "accepted",
		"episode_id": episodeID,
		"message":    "episode enqueued for cloud sync",
	})
}

// ListSyncJobs lists sync log entries with filtering and pagination.
//
// @Summary      List sync jobs
// @Description  Returns sync log entries with optional status filter
// @Tags         sync
// @Produce      json
// @Param        status  query     string  false  "Filter by status (pending, in_progress, completed, failed)"
// @Param        limit   query     int     false  "Max results (default 50)"
// @Param        offset  query     int     false  "Pagination offset (default 0)"
// @Success      200     {object}  SyncJobListResponse
// @Failure      500     {object}  map[string]string
// @Router       /sync/episodes [get]
func (h *SyncHandler) ListSyncJobs(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	status := strings.TrimSpace(c.Query("status"))

	whereClause := "WHERE 1=1"
	args := []interface{}{}

	if status != "" {
		whereClause += " AND sl.status = ?"
		args = append(args, status)
	}

	// Count
	countQuery := "SELECT COUNT(*) FROM sync_logs sl " + whereClause
	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[SYNC] Failed to count sync logs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync jobs"})
		return
	}

	// Query
	query := `
		SELECT
			sl.id,
			sl.episode_id,
			e.episode_id AS episode_public_id,
			sl.source_factory_id,
			sl.source_path,
			sl.destination_path,
			sl.status,
			sl.bytes_transferred,
			sl.duration_sec,
			sl.error_message,
			COALESCE(sl.attempt_count, 0) AS attempt_count,
			sl.next_retry_at,
			sl.started_at,
			sl.completed_at
		FROM sync_logs sl
		LEFT JOIN episodes e ON e.id = sl.episode_id AND e.deleted_at IS NULL
		` + whereClause + `
		ORDER BY sl.started_at DESC
		LIMIT ? OFFSET ?
	`
	queryArgs := append(args, pagination.Limit, pagination.Offset)

	var rows []syncLogRow
	// #nosec G201 -- Query is constructed with parameterized inputs
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[SYNC] Failed to query sync logs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync jobs"})
		return
	}

	items := make([]SyncJobResponse, len(rows))
	for i, r := range rows {
		items[i] = SyncJobResponse{
			ID:               r.ID,
			EpisodeID:        r.EpisodeID,
			EpisodePublicID:  nullableString(r.EpisodePublicID),
			SourcePath:       nullableString(r.SourcePath),
			DestinationPath:  nullableString(r.DestinationPath),
			Status:           r.Status,
			BytesTransferred: nullableInt64(r.BytesTransferred),
			DurationSec:      nullableInt64(r.DurationSec),
			ErrorMessage:     nullableString(r.ErrorMessage),
			AttemptCount:     r.AttemptCount,
			NextRetryAt:      nullableTime(r.NextRetryAt),
			StartedAt:        nullableTime(r.StartedAt),
			CompletedAt:      nullableTime(r.CompletedAt),
		}
	}

	hasNext := (pagination.Offset + pagination.Limit) < total
	hasPrev := pagination.Offset > 0

	c.JSON(http.StatusOK, SyncJobListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: hasNext,
		HasPrev: hasPrev,
	})
}

// GetSyncStatus returns the sync status for a specific episode.
//
// @Summary      Get episode sync status
// @Description  Returns the latest sync log entry for a specific episode
// @Tags         sync
// @Produce      json
// @Param        id   path      int  true  "Episode ID"
// @Success      200  {object}  SyncJobResponse
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /sync/episodes/{id}/status [get]
func (h *SyncHandler) GetSyncStatus(c *gin.Context) {
	idStr := c.Param("id")
	episodeID, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || episodeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}

	var row syncLogRow
	err = h.db.Get(&row, `
		SELECT
			sl.id,
			sl.episode_id,
			e.episode_id AS episode_public_id,
			sl.source_factory_id,
			sl.source_path,
			sl.destination_path,
			sl.status,
			sl.bytes_transferred,
			sl.duration_sec,
			sl.error_message,
			COALESCE(sl.attempt_count, 0) AS attempt_count,
			sl.next_retry_at,
			sl.started_at,
			sl.completed_at
		FROM sync_logs sl
		LEFT JOIN episodes e ON e.id = sl.episode_id AND e.deleted_at IS NULL
		WHERE sl.episode_id = ?
		ORDER BY sl.started_at DESC
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no sync record found for this episode"})
		return
	}
	if err != nil {
		logger.Printf("[SYNC] Failed to query sync status for episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get sync status"})
		return
	}

	c.JSON(http.StatusOK, SyncJobResponse{
		ID:               row.ID,
		EpisodeID:        row.EpisodeID,
		EpisodePublicID:  nullableString(row.EpisodePublicID),
		SourcePath:       nullableString(row.SourcePath),
		DestinationPath:  nullableString(row.DestinationPath),
		Status:           row.Status,
		BytesTransferred: nullableInt64(row.BytesTransferred),
		DurationSec:      nullableInt64(row.DurationSec),
		ErrorMessage:     nullableString(row.ErrorMessage),
		AttemptCount:     row.AttemptCount,
		NextRetryAt:      nullableTime(row.NextRetryAt),
		StartedAt:        nullableTime(row.StartedAt),
		CompletedAt:      nullableTime(row.CompletedAt),
	})
}

// GetSyncConfig returns the current sync configuration (sanitized).
//
// @Summary      Get sync configuration
// @Description  Returns current cloud sync configuration (secrets redacted)
// @Tags         sync
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /sync/config [get]
func (h *SyncHandler) GetSyncConfig(c *gin.Context) {
	workerRunning := false
	if h.syncWorker != nil {
		workerRunning = h.syncWorker.IsRunning()
	}
	c.JSON(http.StatusOK, gin.H{
		"worker_running": workerRunning,
	})
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	val := v.Int64
	return &val
}
