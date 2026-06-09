// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/minio/minio-go/v7"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"
)

const (
	episodeQACheckMcapMagic = "mcap_magic"

	qaRunModeAuto   QARunMode = "auto"
	qaRunModeManual QARunMode = "manual"

	qaStatusPendingQA         = "pending_qa"
	qaStatusRunning           = "qa_running"
	qaStatusApproved          = "approved"
	qaStatusNeedsInspection   = "needs_inspection"
	qaStatusInspectorApproved = "inspector_approved"
	qaStatusRejected          = "rejected"
	qaStatusFailed            = "failed"

	defaultEpisodeQAQueueSize = 256
	defaultEpisodeQATimeout   = 2 * time.Minute
)

var (
	mcapMagicBytes = []byte{0x89, 0x4d, 0x43, 0x41, 0x50, 0x30, 0x0d, 0x0a}

	errEpisodeQANotFound       = errors.New("episode not found")
	errEpisodeQAAlreadyRunning = errors.New("episode qa already running")
	errEpisodeQAAutoSkipped    = errors.New("episode auto qa skipped")
)

// QARunMode identifies whether a suite run was triggered automatically or manually.
type QARunMode string

// EpisodeQAHandler handles QA center APIs and lightweight automatic QA execution.
type EpisodeQAHandler struct {
	db      *sqlx.DB
	s3      *s3.Client
	bucket  string
	authCfg *config.AuthConfig
	queue   chan int64
}

// EpisodeQARunRequest is the request body for running an episode QA suite.
type EpisodeQARunRequest struct {
	Mode QARunMode `json:"mode,omitempty" example:"manual"`
}

// EpisodeQACheckRecordResponse is one persisted QA check result.
type EpisodeQACheckRecordResponse struct {
	ID            int64          `json:"id,omitempty"`
	EpisodeID     int64          `json:"episode_id,omitempty"`
	CheckName     string         `json:"check_name"`
	Passed        bool           `json:"passed"`
	Score         float64        `json:"score"`
	Details       string         `json:"details"`
	CheckMetadata map[string]any `json:"check_metadata,omitempty"`
	CheckedAt     string         `json:"checked_at"`
}

// EpisodeQASuiteResponse is the response for a full episode QA suite run.
type EpisodeQASuiteResponse struct {
	EpisodeID int64                          `json:"episode_id"`
	QAStatus  string                         `json:"qa_status"`
	Passed    bool                           `json:"passed"`
	Mode      QARunMode                      `json:"mode"`
	Checks    []EpisodeQACheckRecordResponse `json:"checks"`
}

// EpisodeQAEpisodeResponse is one row in the QA center episode list.
type EpisodeQAEpisodeResponse struct {
	ID            int64                         `json:"id"`
	PublicID      string                        `json:"public_id"`
	EpisodeID     string                        `json:"episode_id"`
	TaskID        int64                         `json:"task_id"`
	TaskPublicID  *string                       `json:"task_public_id,omitempty"`
	RobotType     *string                       `json:"robot_type,omitempty"`
	QAStatus      string                        `json:"qa_status"`
	QualityFlag   *string                       `json:"quality_flag,omitempty"`
	CreatedAt     string                        `json:"created_at"`
	LatestQACheck *EpisodeQACheckRecordResponse `json:"latest_qa_check,omitempty"`
}

// EpisodeQAPaginationResponse describes page-based QA center pagination.
type EpisodeQAPaginationResponse struct {
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
}

// EpisodeQAListResponse is the QA center episode list response.
type EpisodeQAListResponse struct {
	Items      []EpisodeQAEpisodeResponse  `json:"items"`
	Pagination EpisodeQAPaginationResponse `json:"pagination"`
	Total      int                         `json:"total"`
	Limit      int                         `json:"limit"`
	Offset     int                         `json:"offset"`
	HasNext    bool                        `json:"hasNext,omitempty"`
	HasPrev    bool                        `json:"hasPrev,omitempty"`
}

type episodeQACheckOutcome struct {
	CheckName string
	Passed    bool
	Score     float64
	Details   string
	Metadata  map[string]any
}

type episodeQACheckRow struct {
	ID       int64          `db:"id"`
	McapPath string         `db:"mcap_path"`
	QAStatus string         `db:"qa_status"`
	Quality  sql.NullString `db:"quality_flag"`
}

type episodeQARunClaim struct {
	EpisodeID      int64
	OriginalStatus string
	MutableStatus  bool
}

type episodeQACheckDBRow struct {
	ID            int64          `db:"id"`
	EpisodeID     int64          `db:"episode_id"`
	CheckName     string         `db:"check_name"`
	Passed        bool           `db:"passed"`
	Score         float64        `db:"score"`
	Details       sql.NullString `db:"details"`
	CheckMetadata sql.NullString `db:"check_metadata"`
	CheckedAt     sql.NullTime   `db:"checked_at"`
}

type episodeQAListRow struct {
	ID                   int64           `db:"id"`
	EpisodeID            string          `db:"episode_id"`
	TaskID               int64           `db:"task_id"`
	TaskPublicID         sql.NullString  `db:"task_public_id"`
	RobotType            sql.NullString  `db:"robot_type"`
	QAStatus             string          `db:"qa_status"`
	QualityFlag          sql.NullString  `db:"quality_flag"`
	CreatedAt            time.Time       `db:"created_at"`
	LatestCheckID        sql.NullInt64   `db:"latest_check_id"`
	LatestCheckName      sql.NullString  `db:"latest_check_name"`
	LatestCheckPassed    sql.NullBool    `db:"latest_check_passed"`
	LatestCheckScore     sql.NullFloat64 `db:"latest_check_score"`
	LatestCheckDetails   sql.NullString  `db:"latest_check_details"`
	LatestCheckMetadata  sql.NullString  `db:"latest_check_metadata"`
	LatestCheckCheckedAt sql.NullTime    `db:"latest_check_checked_at"`
}

// NewEpisodeQAHandler creates the QA handler and starts the in-memory auto-QA worker.
func NewEpisodeQAHandler(db *sqlx.DB, s3Client *s3.Client, bucket string, authCfg *config.AuthConfig) *EpisodeQAHandler {
	h := &EpisodeQAHandler{
		db:      db,
		s3:      s3Client,
		bucket:  strings.TrimSpace(bucket),
		authCfg: authCfg,
		queue:   make(chan int64, defaultEpisodeQAQueueSize),
	}
	if db != nil {
		go h.runAutoWorker()
	}
	return h
}

// RegisterRoutes registers QA center routes under /api/v1/qa.
func (h *EpisodeQAHandler) RegisterRoutes(apiV1 *gin.RouterGroup) {
	qa := apiV1.Group("/qa")
	qa.GET("/episodes", h.ListQAEpisodes)
	qa.GET("/episodes/:id/checks", h.ListEpisodeQAChecks)
	qa.POST("/episodes/:id/run", h.RunEpisodeQASuiteHTTP)
}

// EnqueueEpisode schedules lightweight automatic QA for a newly created episode.
func (h *EpisodeQAHandler) EnqueueEpisode(episodeID int64) {
	if h == nil || h.queue == nil || episodeID <= 0 {
		return
	}
	select {
	case h.queue <- episodeID:
	default:
		logger.Printf("[EPISODE-QA] Auto QA queue full, dropped episode=%d", episodeID)
	}
}

func (h *EpisodeQAHandler) runAutoWorker() {
	for episodeID := range h.queue {
		ctx, cancel := context.WithTimeout(context.Background(), defaultEpisodeQATimeout)
		if _, err := h.RunEpisodeQASuite(ctx, episodeID, qaRunModeAuto); err != nil && !errors.Is(err, errEpisodeQAAutoSkipped) {
			logger.Printf("[EPISODE-QA] Auto QA failed: episode=%d, err=%v", episodeID, err)
		}
		cancel()
	}
}

func (h *EpisodeQAHandler) requireBearerJWT(c *gin.Context) bool {
	if h.authCfg == nil {
		return true
	}
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authorization header"})
		return false
	}
	if _, err := auth.ParseToken(parts[1], h.authCfg); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return false
	}
	return true
}

// ListQAEpisodes lists episodes for the QA center.
//
// @Summary      List QA center episodes
// @Description  Lists episodes with latest QA check. Defaults to actionable statuses.
// @Tags         qa
// @Produce      json
// @Param        status      query     string  false  "QA status filter: all, pending_qa, failed, needs_inspection, approved, rejected"
// @Param        robot_type  query     string  false  "Robot type name or model"
// @Param        q           query     string  false  "Search episode/task/quality text"
// @Param        page        query     int     false  "Page number, default 1"
// @Param        page_size   query     int     false  "Page size, default 20"
// @Success      200         {object}  EpisodeQAListResponse
// @Failure      400         {object}  map[string]string
// @Failure      500         {object}  map[string]string
// @Router       /qa/episodes [get]
func (h *EpisodeQAHandler) ListQAEpisodes(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return
	}
	if !h.requireBearerJWT(c) {
		return
	}

	page := parsePositiveIntQuery(c, "page", 1)
	pageSize := parsePositiveIntQuery(c, "page_size", 20)
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize

	statuses, err := qaEpisodeStatusFilter(c.DefaultQuery("status", ""))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	where, args := buildQAEpisodeListWhere(statuses, c.Query("robot_type"), c.Query("q"))
	countQuery := `
		SELECT COUNT(1)
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
	` + where

	var total int
	if err := h.db.Get(&total, countQuery, args...); err != nil {
		logger.Printf("[EPISODE-QA] Failed to count QA episodes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count qa episodes"})
		return
	}

	query := `
		SELECT
			e.id,
			e.episode_id,
			e.task_id,
			t.task_id AS task_public_id,
			COALESCE(rt.name, rt.model, '') AS robot_type,
			COALESCE(e.qa_status, '') AS qa_status,
			e.quality_flag,
			e.created_at,
			qc.id AS latest_check_id,
			qc.check_name AS latest_check_name,
			qc.passed AS latest_check_passed,
			qc.score AS latest_check_score,
			qc.details AS latest_check_details,
			qc.check_metadata AS latest_check_metadata,
			qc.checked_at AS latest_check_checked_at
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id = e.workstation_id AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		LEFT JOIN robot_types rt ON rt.id = r.robot_type_id AND rt.deleted_at IS NULL
		LEFT JOIN qa_checks qc ON qc.id = (
			SELECT qc2.id
			FROM qa_checks qc2
			WHERE qc2.episode_id = e.id
			ORDER BY qc2.checked_at DESC, qc2.id DESC
			LIMIT 1
		)
	` + where + `
		ORDER BY e.created_at DESC, e.id DESC
		LIMIT ? OFFSET ?
	`
	queryArgs := append(append([]interface{}{}, args...), pageSize, offset)

	var rows []episodeQAListRow
	if err := h.db.Select(&rows, query, queryArgs...); err != nil {
		logger.Printf("[EPISODE-QA] Failed to query QA episodes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query qa episodes"})
		return
	}

	items := make([]EpisodeQAEpisodeResponse, 0, len(rows))
	for _, row := range rows {
		item := EpisodeQAEpisodeResponse{
			ID:           row.ID,
			PublicID:     row.EpisodeID,
			EpisodeID:    row.EpisodeID,
			TaskID:       row.TaskID,
			TaskPublicID: nullableString(row.TaskPublicID),
			RobotType:    nullableString(row.RobotType),
			QAStatus:     row.QAStatus,
			QualityFlag:  nullableString(row.QualityFlag),
			CreatedAt:    row.CreatedAt.UTC().Format(time.RFC3339),
		}
		if row.LatestCheckID.Valid {
			item.LatestQACheck = latestQACheckFromListRow(row)
		}
		items = append(items, item)
	}

	hasNext := offset+pageSize < total
	c.JSON(http.StatusOK, EpisodeQAListResponse{
		Items: items,
		Pagination: EpisodeQAPaginationResponse{
			Page:     page,
			PageSize: pageSize,
			Total:    total,
		},
		Total:   total,
		Limit:   pageSize,
		Offset:  offset,
		HasNext: hasNext,
		HasPrev: page > 1,
	})
}

// ListEpisodeQAChecks lists all QA check records for one episode.
//
// @Summary      List episode QA checks
// @Description  Lists persisted QA check history for one episode.
// @Tags         qa
// @Produce      json
// @Param        id   path      int  true  "Episode ID"
// @Success      200  {object}  map[string][]EpisodeQACheckRecordResponse
// @Failure      404  {object}  map[string]string
// @Failure      500  {object}  map[string]string
// @Router       /qa/episodes/{id}/checks [get]
func (h *EpisodeQAHandler) ListEpisodeQAChecks(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return
	}
	if !h.requireBearerJWT(c) {
		return
	}

	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}
	if err := h.ensureEpisodeExists(c.Request.Context(), episodeID); err != nil {
		if errors.Is(err, errEpisodeQANotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
			return
		}
		logger.Printf("[EPISODE-QA] Failed to query episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episode"})
		return
	}

	var rows []episodeQACheckDBRow
	if err := h.db.SelectContext(c.Request.Context(), &rows, `
		SELECT id, episode_id, check_name, passed, score, details, check_metadata, checked_at
		FROM qa_checks
		WHERE episode_id = ?
		ORDER BY checked_at DESC, id DESC
	`, episodeID); err != nil {
		logger.Printf("[EPISODE-QA] Failed to query QA checks: episode=%d, err=%v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query qa checks"})
		return
	}

	items := make([]EpisodeQACheckRecordResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, qaCheckRecordFromDBRow(row))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// RunEpisodeQASuiteHTTP runs the full QA suite for one episode.
//
// @Summary      Run episode QA suite
// @Description  Runs the configured QA suite for one episode. The MVP suite is mcap_magic.
// @Tags         qa
// @Accept       json
// @Produce      json
// @Param        id       path      int                  true  "Episode ID"
// @Param        request  body      EpisodeQARunRequest  false "QA run request"
// @Success      200      {object}  EpisodeQASuiteResponse
// @Failure      404      {object}  map[string]string
// @Failure      409      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Failure      502      {object}  map[string]string
// @Router       /qa/episodes/{id}/run [post]
func (h *EpisodeQAHandler) RunEpisodeQASuiteHTTP(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return
	}
	if !h.requireBearerJWT(c) {
		return
	}

	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}

	var req EpisodeQARunRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid qa run request"})
			return
		}
	}
	mode := req.Mode
	if mode == "" {
		mode = qaRunModeManual
	}
	if mode != qaRunModeManual && mode != qaRunModeAuto {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be manual or auto"})
		return
	}

	result, err := h.RunEpisodeQASuite(c.Request.Context(), episodeID, mode)
	if err != nil {
		switch {
		case errors.Is(err, errEpisodeQANotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		case errors.Is(err, errEpisodeQAAlreadyRunning):
			c.JSON(http.StatusConflict, gin.H{"error": "qa already running"})
		default:
			logger.Printf("[EPISODE-QA] Suite failed: episode=%d, mode=%s, err=%v", episodeID, mode, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to run qa suite"})
		}
		return
	}

	c.JSON(http.StatusOK, result)
}

// RunEpisodeQASuite executes and persists the configured QA suite for one episode.
func (h *EpisodeQAHandler) RunEpisodeQASuite(ctx context.Context, episodeID int64, mode QARunMode) (*EpisodeQASuiteResponse, error) {
	if h == nil || h.db == nil {
		return nil, fmt.Errorf("database is not configured")
	}
	if mode == "" {
		mode = qaRunModeManual
	}

	row, err := h.loadEpisodeForQACheck(ctx, episodeID)
	if err != nil {
		return nil, err
	}

	claim, err := h.claimEpisodeQARun(ctx, row, mode)
	if err != nil {
		return nil, err
	}

	checks := defaultEpisodeQASuite(row)
	outcomes := make([]episodeQACheckOutcome, 0, len(checks))
	checkedAt := time.Now().UTC()
	for _, checkName := range checks {
		outcome, err := h.runEpisodeQACheck(ctx, checkName, row)
		if err != nil {
			h.releaseEpisodeQARun(ctx, claim)
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}

	result, err := h.persistEpisodeQASuiteResult(ctx, claim, mode, outcomes, checkedAt)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func defaultEpisodeQASuite(_ episodeQACheckRow) []string {
	return []string{episodeQACheckMcapMagic}
}

func normalizeEpisodeQACheckName(raw string) string {
	return strings.TrimSpace(strings.ToLower(raw))
}

func isSupportedEpisodeQACheckName(checkName string) bool {
	switch checkName {
	case episodeQACheckMcapMagic:
		return true
	default:
		return false
	}
}

func (h *EpisodeQAHandler) loadEpisodeForQACheck(ctx context.Context, episodeID int64) (episodeQACheckRow, error) {
	var row episodeQACheckRow
	err := h.db.GetContext(ctx, &row, `
		SELECT id, mcap_path, COALESCE(qa_status, '') AS qa_status, quality_flag
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		return row, errEpisodeQANotFound
	}
	if err != nil {
		return row, fmt.Errorf("query episode: %w", err)
	}
	return row, nil
}

func (h *EpisodeQAHandler) ensureEpisodeExists(ctx context.Context, episodeID int64) error {
	var exists int
	err := h.db.GetContext(ctx, &exists, `
		SELECT 1
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		return errEpisodeQANotFound
	}
	if err != nil {
		return fmt.Errorf("query episode: %w", err)
	}
	return nil
}

func (h *EpisodeQAHandler) claimEpisodeQARun(ctx context.Context, row episodeQACheckRow, mode QARunMode) (episodeQARunClaim, error) {
	claim := episodeQARunClaim{
		EpisodeID:      row.ID,
		OriginalStatus: row.QAStatus,
	}

	if row.QAStatus == qaStatusRunning {
		return claim, errEpisodeQAAlreadyRunning
	}
	if mode == qaRunModeAuto && row.QAStatus != qaStatusPendingQA {
		return claim, errEpisodeQAAutoSkipped
	}
	if mode == qaRunModeManual && isManualQAProtectedStatus(row.QAStatus) {
		return claim, nil
	}

	// #nosec G701 -- static SQL with placeholder-bound status and episode values.
	res, err := h.db.ExecContext(ctx, `
		UPDATE episodes
		SET qa_status = ?
		WHERE id = ? AND deleted_at IS NULL AND COALESCE(qa_status, '') = ?
	`, qaStatusRunning, row.ID, row.QAStatus)
	if err != nil {
		return claim, fmt.Errorf("claim episode qa run: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return claim, fmt.Errorf("read claim rows affected: %w", err)
	}
	if affected == 0 {
		fresh, err := h.loadEpisodeForQACheck(ctx, row.ID)
		if err != nil {
			return claim, err
		}
		if fresh.QAStatus == qaStatusRunning {
			return claim, errEpisodeQAAlreadyRunning
		}
		if mode == qaRunModeAuto {
			return claim, errEpisodeQAAutoSkipped
		}
		return claim, fmt.Errorf("episode qa status changed from %q to %q", row.QAStatus, fresh.QAStatus)
	}

	claim.MutableStatus = true
	return claim, nil
}

func (h *EpisodeQAHandler) releaseEpisodeQARun(ctx context.Context, claim episodeQARunClaim) {
	if h == nil || h.db == nil || !claim.MutableStatus {
		return
	}
	// #nosec G701 -- static SQL with placeholder-bound status and episode values.
	if _, err := h.db.ExecContext(ctx, `
		UPDATE episodes
		SET qa_status = ?
		WHERE id = ? AND deleted_at IS NULL AND qa_status = ?
	`, claim.OriginalStatus, claim.EpisodeID, qaStatusRunning); err != nil {
		logger.Printf("[EPISODE-QA] Failed to release QA run: episode=%d, err=%v", claim.EpisodeID, err)
	}
}

func isManualQAProtectedStatus(status string) bool {
	switch status {
	case qaStatusRejected, qaStatusNeedsInspection, qaStatusInspectorApproved:
		return true
	default:
		return false
	}
}

func (h *EpisodeQAHandler) runEpisodeQACheck(ctx context.Context, checkName string, row episodeQACheckRow) (episodeQACheckOutcome, error) {
	checkName = normalizeEpisodeQACheckName(checkName)
	if !isSupportedEpisodeQACheckName(checkName) {
		return episodeQACheckOutcome{}, fmt.Errorf("unsupported qa check %q", checkName)
	}
	switch checkName {
	case episodeQACheckMcapMagic:
		return h.runMcapMagicQACheck(ctx, row)
	default:
		return episodeQACheckOutcome{}, fmt.Errorf("unsupported qa check %q", checkName)
	}
}

func (h *EpisodeQAHandler) runMcapMagicQACheck(ctx context.Context, row episodeQACheckRow) (episodeQACheckOutcome, error) {
	if h.s3 == nil {
		return episodeQACheckOutcome{}, fmt.Errorf("storage is not configured")
	}

	bucket, objectName, ok := resolveEpisodeMcapLocation(h.bucket, row.McapPath)
	if !ok {
		return evaluateMcapMagicCheck(0, nil, nil, "invalid mcap_path"), nil
	}

	stat, err := h.s3.StatObject(ctx, bucket, objectName, minio.StatObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return mcapMagicFailure("MCAP integrity check failed: object not found", map[string]any{
				"bucket": bucket,
				"object": objectName,
			}), nil
		}
		return episodeQACheckOutcome{}, fmt.Errorf("stat mcap object: %w", err)
	}

	size := stat.Size
	if size < int64(len(mcapMagicBytes)*2) {
		return evaluateMcapMagicCheck(size, nil, nil, "file is smaller than 16 bytes"), nil
	}

	head, err := h.readS3ObjectRange(ctx, bucket, objectName, 0, int64(len(mcapMagicBytes)-1))
	if err != nil {
		if isS3NotFound(err) {
			return mcapMagicFailure("MCAP integrity check failed: object not found", map[string]any{
				"bucket":          bucket,
				"object":          objectName,
				"file_size_bytes": size,
			}), nil
		}
		return episodeQACheckOutcome{}, fmt.Errorf("read mcap head: %w", err)
	}

	tailStart := size - int64(len(mcapMagicBytes))
	tail, err := h.readS3ObjectRange(ctx, bucket, objectName, tailStart, size-1)
	if err != nil {
		if isS3NotFound(err) {
			return mcapMagicFailure("MCAP integrity check failed: object not found", map[string]any{
				"bucket":          bucket,
				"object":          objectName,
				"file_size_bytes": size,
			}), nil
		}
		return episodeQACheckOutcome{}, fmt.Errorf("read mcap tail: %w", err)
	}

	return evaluateMcapMagicCheck(size, head, tail, ""), nil
}

func (h *EpisodeQAHandler) readS3ObjectRange(ctx context.Context, bucket, objectName string, start, end int64) ([]byte, error) {
	var opts minio.GetObjectOptions
	if err := opts.SetRange(start, end); err != nil {
		return nil, fmt.Errorf("set range %d-%d: %w", start, end, err)
	}

	obj, err := h.s3.GetObject(ctx, bucket, objectName, opts)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := obj.Close(); err != nil {
			logger.Printf("[EPISODE-QA] S3 object close failed: bucket=%s, object=%s, err=%v", bucket, objectName, err)
		}
	}()

	return io.ReadAll(obj)
}

func evaluateMcapMagicCheck(fileSize int64, head, tail []byte, explicitReason string) episodeQACheckOutcome {
	metadata := map[string]any{
		"expected_magic":   spacedHex(mcapMagicBytes),
		"found_head_magic": spacedHex(head),
		"found_tail_magic": spacedHex(tail),
		"file_size_bytes":  fileSize,
	}

	if explicitReason != "" {
		return mcapMagicFailure("MCAP integrity check failed: "+explicitReason, metadata)
	}

	headOK := bytes.Equal(head, mcapMagicBytes)
	tailOK := bytes.Equal(tail, mcapMagicBytes)
	if headOK && tailOK {
		return episodeQACheckOutcome{
			CheckName: episodeQACheckMcapMagic,
			Passed:    true,
			Score:     1,
			Details:   "MCAP head and tail magic matched",
			Metadata:  metadata,
		}
	}

	reason := "head and tail magic mismatch"
	if headOK {
		reason = "tail magic mismatch"
	} else if tailOK {
		reason = "head magic mismatch"
	}
	return mcapMagicFailure("MCAP integrity check failed: "+reason, metadata)
}

func mcapMagicFailure(details string, metadata map[string]any) episodeQACheckOutcome {
	base := map[string]any{
		"expected_magic":   spacedHex(mcapMagicBytes),
		"found_head_magic": "",
		"found_tail_magic": "",
	}
	for k, v := range metadata {
		base[k] = v
	}
	return episodeQACheckOutcome{
		CheckName: episodeQACheckMcapMagic,
		Passed:    false,
		Score:     0,
		Details:   details,
		Metadata:  base,
	}
}

func isS3NotFound(err error) bool {
	errResp := minio.ToErrorResponse(err)
	return errResp.Code == "NoSuchKey" || errResp.StatusCode == http.StatusNotFound
}

func spacedHex(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	parts := make([]string, len(data))
	for i, b := range data {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, " ")
}

func (h *EpisodeQAHandler) persistEpisodeQASuiteResult(ctx context.Context, claim episodeQARunClaim, mode QARunMode, outcomes []episodeQACheckOutcome, checkedAt time.Time) (*EpisodeQASuiteResponse, error) {
	if h.db == nil {
		return nil, fmt.Errorf("database is not configured")
	}

	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin qa check transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	checks := make([]EpisodeQACheckRecordResponse, 0, len(outcomes))
	allPassed := true
	scoreSum := 0.0
	failureDetails := ""
	for _, outcome := range outcomes {
		if !outcome.Passed {
			allPassed = false
			if failureDetails == "" {
				failureDetails = outcome.Details
			}
		}
		scoreSum += outcome.Score

		metadataJSON, err := json.Marshal(outcome.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal qa check metadata: %w", err)
		}

		// #nosec G701 -- static SQL with placeholder-bound QA check values.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO qa_checks (episode_id, check_name, passed, score, details, check_metadata, checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, claim.EpisodeID, outcome.CheckName, outcome.Passed, outcome.Score, outcome.Details, string(metadataJSON), checkedAt)
		if err != nil {
			return nil, fmt.Errorf("insert qa_check: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read qa_check insert id: %w", err)
		}
		checks = append(checks, EpisodeQACheckRecordResponse{
			ID:            id,
			EpisodeID:     claim.EpisodeID,
			CheckName:     outcome.CheckName,
			Passed:        outcome.Passed,
			Score:         outcome.Score,
			Details:       outcome.Details,
			CheckMetadata: outcome.Metadata,
			CheckedAt:     checkedAt.Format(time.RFC3339),
		})
	}

	score := 0.0
	if len(outcomes) > 0 {
		score = scoreSum / float64(len(outcomes))
	}

	finalStatus := claim.OriginalStatus
	if allPassed {
		finalStatus = qaStatusApproved
	} else if failureDetails != "" {
		finalStatus = qaStatusFailed
	}

	if claim.MutableStatus {
		if allPassed {
			if mode == qaRunModeAuto {
				// #nosec G701 -- static SQL with placeholder-bound episode QA values.
				if _, err := tx.ExecContext(ctx, `
					UPDATE episodes
					SET qa_status = ?, qa_score = ?, quality_flag = NULL, auto_approved = ?
					WHERE id = ? AND deleted_at IS NULL AND qa_status = ?
				`, qaStatusApproved, score, 1, claim.EpisodeID, qaStatusRunning); err != nil {
					return nil, fmt.Errorf("mark episode qa approved: %w", err)
				}
			} else {
				// #nosec G701 -- static SQL with placeholder-bound episode QA values.
				if _, err := tx.ExecContext(ctx, `
					UPDATE episodes
					SET qa_status = ?, qa_score = ?, quality_flag = NULL
					WHERE id = ? AND deleted_at IS NULL AND qa_status = ?
				`, qaStatusApproved, score, claim.EpisodeID, qaStatusRunning); err != nil {
					return nil, fmt.Errorf("mark episode qa approved: %w", err)
				}
			}
		} else {
			// #nosec G701 -- static SQL with placeholder-bound episode QA values.
			if _, err := tx.ExecContext(ctx, `
				UPDATE episodes
				SET qa_status = ?, qa_score = ?, quality_flag = ?
				WHERE id = ? AND deleted_at IS NULL AND qa_status = ?
			`, qaStatusFailed, score, failureDetails, claim.EpisodeID, qaStatusRunning); err != nil {
				return nil, fmt.Errorf("mark episode qa failed: %w", err)
			}
		}
	} else {
		if failureDetails != "" {
			// #nosec G701 -- static SQL with placeholder-bound episode QA values.
			if _, err := tx.ExecContext(ctx, `
				UPDATE episodes
				SET qa_score = ?, quality_flag = ?
				WHERE id = ? AND deleted_at IS NULL
			`, score, failureDetails, claim.EpisodeID); err != nil {
				return nil, fmt.Errorf("write protected episode qa failure details: %w", err)
			}
		}
		finalStatus = claim.OriginalStatus
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit qa check transaction: %w", err)
	}

	return &EpisodeQASuiteResponse{
		EpisodeID: claim.EpisodeID,
		QAStatus:  finalStatus,
		Passed:    allPassed,
		Mode:      mode,
		Checks:    checks,
	}, nil
}

func parsePositiveIntQuery(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func qaEpisodeStatusFilter(raw string) ([]string, error) {
	status := strings.TrimSpace(strings.ToLower(raw))
	if status == "" {
		return []string{qaStatusPendingQA, qaStatusFailed, qaStatusNeedsInspection}, nil
	}
	if status == "all" {
		return nil, nil
	}
	if status == qaStatusApproved {
		return []string{qaStatusApproved, qaStatusInspectorApproved}, nil
	}

	parts := strings.Split(status, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		switch s {
		case qaStatusPendingQA, qaStatusRunning, qaStatusFailed, qaStatusNeedsInspection, qaStatusInspectorApproved, qaStatusRejected:
			out = append(out, s)
		default:
			return nil, fmt.Errorf("unsupported qa status %q", s)
		}
	}
	return out, nil
}

func buildQAEpisodeListWhere(statuses []string, robotType, keyword string) (string, []interface{}) {
	where := " WHERE e.deleted_at IS NULL"
	args := []interface{}{}

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, status := range statuses {
			placeholders[i] = "?"
			args = append(args, status)
		}
		where += " AND e.qa_status IN (" + strings.Join(placeholders, ",") + ")"
	}

	rt := strings.TrimSpace(robotType)
	if rt != "" {
		where += " AND (rt.name = ? OR rt.model = ?)"
		args = append(args, rt, rt)
	}

	q := strings.TrimSpace(keyword)
	if q != "" {
		like := "%" + q + "%"
		where += " AND (e.episode_id LIKE ? OR t.task_id LIKE ? OR e.quality_flag LIKE ?)"
		args = append(args, like, like, like)
	}

	return where, args
}

func latestQACheckFromListRow(row episodeQAListRow) *EpisodeQACheckRecordResponse {
	checkedAt := ""
	if row.LatestCheckCheckedAt.Valid {
		checkedAt = row.LatestCheckCheckedAt.Time.UTC().Format(time.RFC3339)
	}
	return &EpisodeQACheckRecordResponse{
		ID:            row.LatestCheckID.Int64,
		EpisodeID:     row.ID,
		CheckName:     row.LatestCheckName.String,
		Passed:        row.LatestCheckPassed.Valid && row.LatestCheckPassed.Bool,
		Score:         nullFloat64Value(row.LatestCheckScore),
		Details:       nullStringValue(row.LatestCheckDetails),
		CheckMetadata: parseQACheckMetadata(row.LatestCheckMetadata),
		CheckedAt:     checkedAt,
	}
}

func qaCheckRecordFromDBRow(row episodeQACheckDBRow) EpisodeQACheckRecordResponse {
	checkedAt := ""
	if row.CheckedAt.Valid {
		checkedAt = row.CheckedAt.Time.UTC().Format(time.RFC3339)
	}
	return EpisodeQACheckRecordResponse{
		ID:            row.ID,
		EpisodeID:     row.EpisodeID,
		CheckName:     row.CheckName,
		Passed:        row.Passed,
		Score:         row.Score,
		Details:       nullStringValue(row.Details),
		CheckMetadata: parseQACheckMetadata(row.CheckMetadata),
		CheckedAt:     checkedAt,
	}
}

func parseQACheckMetadata(raw sql.NullString) map[string]any {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return nil
	}
	return out
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func nullFloat64Value(value sql.NullFloat64) float64 {
	if !value.Valid {
		return 0
	}
	return value.Float64
}
