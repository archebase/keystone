// Package handlers provides HTTP request handlers for Keystone Edge API
package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// EpisodeHandler handles episode-related HTTP requests
type EpisodeHandler struct {
	db *sql.DB
}

// NewEpisodeHandler creates a new EpisodeHandler
func NewEpisodeHandler(db *sql.DB) *EpisodeHandler {
	return &EpisodeHandler{
		db: db,
	}
}

// Episode represents an episode in the API response
type Episode struct {
	ID             string  `json:"id"`
	TaskID         string  `json:"task_id"`
	McapPath       string  `json:"mcap_path"`
	SidecarPath    string  `json:"sidecar_path"`
	QaStatus       string  `json:"qa_status"`
	QaScore        float64 `json:"qa_score"`
	AutoApproved   bool    `json:"auto_approved"`
	CloudProcessed bool    `json:"cloud_processed"`
	CreatedAt      string  `json:"created_at"`
}

// EpisodeListResponse represents the response for listing episodes
type EpisodeListResponse struct {
	Episodes []Episode `json:"episodes"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

// RegisterRoutes registers episode-related routes
func (h *EpisodeHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	apiV1.GET("", h.ListEpisodes)
}

// ListEpisodes returns a list of episodes with filtering and pagination
//
// @Summary      List episodes
// @Description  Returns a list of episodes with optional filtering by task_id, qa_status, auto_approved, and cloud_processed
// @Tags         episodes
// @Produce      json
// @Param        task_id          query     string  false  "Filter by task ID"
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
			COALESCE(t.task_id, '') as task_id,
			e.mcap_path,
			e.sidecar_path,
			COALESCE(e.qa_status, '') as qa_status,
			COALESCE(e.qa_score, 0) as qa_score,
			e.auto_approved,
			e.cloud_processed,
			e.created_at
		FROM episodes e
		LEFT JOIN tasks t ON e.task_id = t.id
		WHERE e.deleted_at IS NULL
	`

	countQuery := `
		SELECT COUNT(1)
		FROM episodes e
		LEFT JOIN tasks t ON e.task_id = t.id
		WHERE e.deleted_at IS NULL
	`

	args := []interface{}{}
	argsCount := []interface{}{}

	// Add filters
	if taskID != "" {
		query += " AND t.task_id = ?"
		countQuery += " AND t.task_id = ?"
		args = append(args, taskID)
		argsCount = append(argsCount, taskID)
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
	// #nosec G201 -- Query is constructed with parameterized inputs
	err = h.db.QueryRow(countQuery, argsCount...).Scan(&total)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count episodes"})
		return
	}

	// Add ordering and pagination
	query += " ORDER BY e.created_at DESC"
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	// Execute query
	// #nosec G201 -- Query is constructed with parameterized inputs
	rows, err := h.db.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episodes"})
		return
	}
	defer func() { _ = rows.Close() }()

	// Parse results
	episodes := []Episode{}
	for rows.Next() {
		var ep Episode
		var createdAt time.Time
		err := rows.Scan(
			&ep.ID,
			&ep.TaskID,
			&ep.McapPath,
			&ep.SidecarPath,
			&ep.QaStatus,
			&ep.QaScore,
			&ep.AutoApproved,
			&ep.CloudProcessed,
			&createdAt,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to scan episode row: " + err.Error()})
			return
		}
		ep.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		episodes = append(episodes, ep)
	}

	if err = rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to iterate episode rows: " + err.Error()})
		return
	}

	// Return response
	c.JSON(http.StatusOK, EpisodeListResponse{
		Episodes: episodes,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	})
}
