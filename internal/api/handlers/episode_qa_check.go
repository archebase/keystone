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
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"

	"archebase.com/keystone-edge/internal/logger"
)

const (
	episodeQACheckMcapMagic = "mcap_magic"
)

var mcapMagicBytes = []byte{0x89, 0x4d, 0x43, 0x41, 0x50, 0x30, 0x0d, 0x0a}

type episodeQACheckRequest struct {
	CheckName string `json:"check_name"`
}

// EpisodeQACheckResponse is the API response for a single episode QA check run.
type EpisodeQACheckResponse struct {
	EpisodeID     int64          `json:"episode_id"`
	CheckName     string         `json:"check_name"`
	Passed        bool           `json:"passed"`
	Score         float64        `json:"score"`
	Details       string         `json:"details"`
	CheckMetadata map[string]any `json:"check_metadata"`
	CheckedAt     string         `json:"checked_at"`
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
	QaStatus string         `db:"qa_status"`
	Quality  sql.NullString `db:"quality_flag"`
}

// RunEpisodeQACheck executes one QA check for an episode and persists its result.
//
// @Summary      Run episode QA check
// @Description  Runs a single QA check for an episode. Currently supports mcap_magic.
// @Tags         episodes
// @Accept       json
// @Produce      json
// @Param        id       path      int                    true  "Episode ID"
// @Param        request  body      episodeQACheckRequest  true  "QA check request"
// @Success      200      {object}  EpisodeQACheckResponse
// @Failure      400      {object}  map[string]string
// @Failure      404      {object}  map[string]string
// @Failure      500      {object}  map[string]string
// @Failure      502      {object}  map[string]string
// @Router       /episodes/{id}/qa-checks [post]
func (h *EpisodeHandler) RunEpisodeQACheck(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not configured"})
		return
	}
	if h.authCfg != nil && !h.requireBearerJWT(c) {
		return
	}

	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}

	var req episodeQACheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid qa check request"})
		return
	}
	checkName := normalizeEpisodeQACheckName(req.CheckName)
	if checkName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "check_name is required"})
		return
	}
	if !isSupportedEpisodeQACheckName(checkName) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported qa check"})
		return
	}

	row, ok := h.loadEpisodeForQACheck(c, episodeID)
	if !ok {
		return
	}

	outcome, err := h.runEpisodeQACheck(c.Request.Context(), checkName, row)
	if err != nil {
		logger.Printf("[EPISODE-QA] Check failed before result: episode=%d, check=%s, err=%v", episodeID, checkName, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to run qa check"})
		return
	}

	checkedAt := time.Now().UTC()
	if err := h.persistEpisodeQACheckResult(c.Request.Context(), row.ID, outcome, checkedAt); err != nil {
		logger.Printf("[EPISODE-QA] Persist check failed: episode=%d, check=%s, err=%v", episodeID, checkName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist qa check"})
		return
	}

	c.JSON(http.StatusOK, episodeQACheckResponse(row.ID, outcome, checkedAt))
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

func (h *EpisodeHandler) loadEpisodeForQACheck(c *gin.Context, episodeID int64) (episodeQACheckRow, bool) {
	var row episodeQACheckRow
	err := h.db.Get(&row, `
		SELECT id, mcap_path, COALESCE(qa_status, '') AS qa_status, quality_flag
		FROM episodes
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		return row, false
	}
	if err != nil {
		logger.Printf("[EPISODE-QA] Failed to query episode %d: %v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query episode"})
		return row, false
	}
	return row, true
}

func (h *EpisodeHandler) runEpisodeQACheck(ctx context.Context, checkName string, row episodeQACheckRow) (episodeQACheckOutcome, error) {
	switch checkName {
	case episodeQACheckMcapMagic:
		return h.runMcapMagicQACheck(ctx, row)
	default:
		return episodeQACheckOutcome{}, fmt.Errorf("unsupported qa check %q", checkName)
	}
}

func (h *EpisodeHandler) runMcapMagicQACheck(ctx context.Context, row episodeQACheckRow) (episodeQACheckOutcome, error) {
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

func (h *EpisodeHandler) readS3ObjectRange(ctx context.Context, bucket, objectName string, start, end int64) ([]byte, error) {
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

func (h *EpisodeHandler) persistEpisodeQACheckResult(ctx context.Context, episodeID int64, outcome episodeQACheckOutcome, checkedAt time.Time) error {
	if h.db == nil {
		return fmt.Errorf("database is not configured")
	}

	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin qa check transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	metadataJSON, err := json.Marshal(outcome.Metadata)
	if err != nil {
		return fmt.Errorf("marshal qa check metadata: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO qa_checks (episode_id, check_name, passed, score, details, check_metadata, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, episodeID, outcome.CheckName, outcome.Passed, outcome.Score, outcome.Details, string(metadataJSON), checkedAt); err != nil {
		return fmt.Errorf("insert qa_check: %w", err)
	}

	if !outcome.Passed {
		if _, err := tx.ExecContext(ctx, `
			UPDATE episodes
			SET qa_status = 'failed',
			    quality_flag = ?
			WHERE id = ? AND deleted_at IS NULL
		`, outcome.Details, episodeID); err != nil {
			return fmt.Errorf("mark episode qa failed: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit qa check transaction: %w", err)
	}
	return nil
}

func episodeQACheckResponse(episodeID int64, outcome episodeQACheckOutcome, checkedAt time.Time) EpisodeQACheckResponse {
	return EpisodeQACheckResponse{
		EpisodeID:     episodeID,
		CheckName:     outcome.CheckName,
		Passed:        outcome.Passed,
		Score:         outcome.Score,
		Details:       outcome.Details,
		CheckMetadata: outcome.Metadata,
		CheckedAt:     checkedAt.Format(time.RFC3339),
	}
}
