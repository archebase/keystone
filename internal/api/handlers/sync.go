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
	apiV1.GET("/sync/episodes/summary", h.ListEpisodeSyncSummaries)
	apiV1.GET("/sync/episodes/:id/logs", h.ListEpisodeSyncLogs)
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

// syncEpisodeSummaryRow represents the latest sync state grouped by episode.
type syncEpisodeSummaryRow struct {
	ID                 int64          `db:"id"`
	EpisodeID          int64          `db:"episode_id"`
	EpisodePublicID    sql.NullString `db:"episode_public_id"`
	SourceFactoryID    sql.NullString `db:"source_factory_id"`
	SourcePath         sql.NullString `db:"source_path"`
	DestinationPath    sql.NullString `db:"destination_path"`
	Status             string         `db:"status"`
	BytesTransferred   sql.NullInt64  `db:"bytes_transferred"`
	DurationSec        sql.NullInt64  `db:"duration_sec"`
	ErrorMessage       sql.NullString `db:"error_message"`
	TotalAttemptCount  int            `db:"total_attempt_count"`
	LatestAttemptCount int            `db:"latest_attempt_count"`
	SyncLogCount       int            `db:"sync_log_count"`
	NextRetryAt        sql.NullTime   `db:"next_retry_at"`
	StartedAt          sql.NullTime   `db:"started_at"`
	CompletedAt        sql.NullTime   `db:"completed_at"`
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

// SyncEpisodeSummaryResponse represents an episode-centered sync summary.
type SyncEpisodeSummaryResponse struct {
	ID                 int64   `json:"id"`
	EpisodeID          int64   `json:"episode_id"`
	EpisodePublicID    *string `json:"episode_public_id,omitempty"`
	SourcePath         *string `json:"source_path,omitempty"`
	DestinationPath    *string `json:"destination_path,omitempty"`
	Status             string  `json:"status"`
	BytesTransferred   *int64  `json:"bytes_transferred,omitempty"`
	DurationSec        *int64  `json:"duration_sec,omitempty"`
	ErrorMessage       *string `json:"error_message,omitempty"`
	TotalAttemptCount  int     `json:"total_attempt_count"`
	LatestAttemptCount int     `json:"latest_attempt_count"`
	SyncLogCount       int     `json:"sync_log_count"`
	NextRetryAt        *string `json:"next_retry_at,omitempty"`
	StartedAt          *string `json:"started_at,omitempty"`
	CompletedAt        *string `json:"completed_at,omitempty"`
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

// SyncEpisodeSummaryListResponse represents the response for episode sync summaries.
type SyncEpisodeSummaryListResponse struct {
	Items   []SyncEpisodeSummaryResponse `json:"items"`
	Total   int                          `json:"total"`
	Limit   int                          `json:"limit"`
	Offset  int                          `json:"offset"`
	HasNext bool                         `json:"hasNext,omitempty"`
	HasPrev bool                         `json:"hasPrev,omitempty"`
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
		if errors.Is(err, services.ErrSyncWorkerNotRunning) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
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
		case errors.Is(err, services.ErrSyncWorkerNotRunning):
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      err.Error(),
				"episode_id": episodeID,
				"status":     "worker_not_running",
			})
			return
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
		items[i] = syncJobResponseFromRow(r)
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

// ListEpisodeSyncSummaries lists latest sync state grouped by episode.
//
// @Summary      List episode sync summaries
// @Description  Returns one row per episode using latest sync log as current state
// @Tags         sync
// @Produce      json
// @Param        status  query     string  false  "Filter by latest status (pending, in_progress, completed, failed)"
// @Param        limit   query     int     false  "Max results (default 50)"
// @Param        offset  query     int     false  "Pagination offset (default 0)"
// @Success      200     {object}  SyncEpisodeSummaryListResponse
// @Failure      500     {object}  map[string]string
// @Router       /sync/episodes/summary [get]
func (h *SyncHandler) ListEpisodeSyncSummaries(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	status := strings.TrimSpace(c.Query("status"))
	whereClause := "WHERE 1=1"
	args := []interface{}{}
	if status != "" {
		whereClause += " AND latest_log.status = ?"
		args = append(args, status)
	}

	latestJoin := `
		FROM sync_logs latest_log
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) latest ON latest_log.episode_id = latest.episode_id AND latest_log.id = latest.latest_id
	`

	countQuery := "SELECT COUNT(*) " + latestJoin + whereClause
	var total int
	// #nosec G201 -- Query only appends fixed SQL fragments; inputs are parameterized.
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[SYNC] Failed to count episode sync summaries: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync summaries"})
		return
	}

	query := `
		SELECT
			latest_log.id,
			latest_log.episode_id,
			e.episode_id AS episode_public_id,
			latest_log.source_factory_id,
			latest_log.source_path,
			latest_log.destination_path,
			latest_log.status,
			latest_log.bytes_transferred,
			latest_log.duration_sec,
			latest_log.error_message,
			COALESCE(agg.total_attempt_count, 0) AS total_attempt_count,
			COALESCE(latest_log.attempt_count, 0) AS latest_attempt_count,
			COALESCE(agg.sync_log_count, 0) AS sync_log_count,
			latest_log.next_retry_at,
			latest_log.started_at,
			latest_log.completed_at
		FROM sync_logs latest_log
		INNER JOIN (
		  SELECT episode_id, MAX(id) AS latest_id
		  FROM sync_logs
		  GROUP BY episode_id
		) latest ON latest_log.episode_id = latest.episode_id AND latest_log.id = latest.latest_id
		INNER JOIN (
		  SELECT
		    episode_id,
		    SUM(COALESCE(attempt_count, 0)) AS total_attempt_count,
		    COUNT(*) AS sync_log_count
		  FROM sync_logs
		  GROUP BY episode_id
		) agg ON agg.episode_id = latest_log.episode_id
		LEFT JOIN episodes e ON e.id = latest_log.episode_id AND e.deleted_at IS NULL
		` + whereClause + `
		ORDER BY latest_log.started_at DESC, latest_log.id DESC
		LIMIT ? OFFSET ?
	`
	queryArgs := append(args, pagination.Limit, pagination.Offset)

	var rows []syncEpisodeSummaryRow
	// #nosec G201 -- Query only appends fixed SQL fragments; inputs are parameterized.
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[SYNC] Failed to query episode sync summaries: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync summaries"})
		return
	}

	items := make([]SyncEpisodeSummaryResponse, len(rows))
	for i, r := range rows {
		items[i] = syncEpisodeSummaryResponseFromRow(r)
	}

	c.JSON(http.StatusOK, SyncEpisodeSummaryListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: (pagination.Offset + pagination.Limit) < total,
		HasPrev: pagination.Offset > 0,
	})
}

// ListEpisodeSyncLogs lists raw sync log entries for one episode.
//
// @Summary      List episode sync log history
// @Description  Returns raw sync log entries for one episode
// @Tags         sync
// @Produce      json
// @Param        id      path      int  true   "Episode ID"
// @Param        limit   query     int  false  "Max results (default 50)"
// @Param        offset  query     int  false  "Pagination offset (default 0)"
// @Success      200     {object}  SyncJobListResponse
// @Failure      400     {object}  map[string]string
// @Failure      500     {object}  map[string]string
// @Router       /sync/episodes/{id}/logs [get]
func (h *SyncHandler) ListEpisodeSyncLogs(c *gin.Context) {
	pagination, err := ParsePagination(c)
	if err != nil {
		PaginationErrorResponse(c, err)
		return
	}

	idStr := c.Param("id")
	episodeID, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || episodeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid episode id"})
		return
	}

	var total int
	if err := h.db.Get(&total, "SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?", episodeID); err != nil {
		logger.Printf("[SYNC] Failed to count sync logs for episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync logs"})
		return
	}

	var rows []syncLogRow
	if err := h.db.Select(&rows, `
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
		ORDER BY sl.id DESC
		LIMIT ? OFFSET ?
	`, episodeID, pagination.Limit, pagination.Offset); err != nil {
		logger.Printf("[SYNC] Failed to query sync logs for episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sync logs"})
		return
	}

	items := make([]SyncJobResponse, len(rows))
	for i, r := range rows {
		items[i] = syncJobResponseFromRow(r)
	}

	c.JSON(http.StatusOK, SyncJobListResponse{
		Items:   items,
		Total:   total,
		Limit:   pagination.Limit,
		Offset:  pagination.Offset,
		HasNext: (pagination.Offset + pagination.Limit) < total,
		HasPrev: pagination.Offset > 0,
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
		ORDER BY sl.id DESC
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

	c.JSON(http.StatusOK, syncJobResponseFromRow(row))
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
	maxRetries := 0
	if h.syncWorker != nil {
		workerRunning = h.syncWorker.IsRunning()
		maxRetries = h.syncWorker.MaxRetries()
	}
	c.JSON(http.StatusOK, gin.H{
		"worker_running": workerRunning,
		"max_retries":    maxRetries,
	})
}

func syncJobResponseFromRow(r syncLogRow) SyncJobResponse {
	return SyncJobResponse{
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

func syncEpisodeSummaryResponseFromRow(r syncEpisodeSummaryRow) SyncEpisodeSummaryResponse {
	return SyncEpisodeSummaryResponse{
		ID:                 r.ID,
		EpisodeID:          r.EpisodeID,
		EpisodePublicID:    nullableString(r.EpisodePublicID),
		SourcePath:         nullableString(r.SourcePath),
		DestinationPath:    nullableString(r.DestinationPath),
		Status:             r.Status,
		BytesTransferred:   nullableInt64(r.BytesTransferred),
		DurationSec:        nullableInt64(r.DurationSec),
		ErrorMessage:       nullableString(r.ErrorMessage),
		TotalAttemptCount:  r.TotalAttemptCount,
		LatestAttemptCount: r.LatestAttemptCount,
		SyncLogCount:       r.SyncLogCount,
		NextRetryAt:        nullableTime(r.NextRetryAt),
		StartedAt:          nullableTime(r.StartedAt),
		CompletedAt:        nullableTime(r.CompletedAt),
	}
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	val := v.Int64
	return &val
}
