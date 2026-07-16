// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"

	"archebase.com/keystone-edge/internal/logger"
)

const (
	dataOpsBulkMP4Timeout = 30 * time.Minute
	dataOpsBulkMP4Script  = "scripts/mcap_to_mp4.py"
)

type dataOpsBulkMP4PreviewRow struct {
	MatchedCount  int64 `db:"matched_count"`
	AutoFailed    int64 `db:"auto_failed_count"`
	EligibleCount int64 `db:"eligible_count"`
}

type dataOpsBulkMP4EpisodeRow struct {
	ID        int64  `db:"id"`
	EpisodeID string `db:"episode_id"`
	McapPath  string `db:"mcap_path"`
	QAStatus  string `db:"qa_status"`
}

// PreviewBulkEpisodeMP4 previews a filtered bulk MP4 export.
func (h *DataOpsHandler) PreviewBulkEpisodeMP4(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, false)
	if !ok {
		return
	}
	preview, err := h.previewBulkEpisodeMP4(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk MP4 preview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview bulk mp4"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// BulkExportEpisodeMP4 starts a filtered asynchronous MP4 ZIP export.
func (h *DataOpsHandler) BulkExportEpisodeMP4(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	if !h.ensureBulkMP4Configured(c) {
		return
	}

	_, q, ok := h.parseBulkEpisodeActionRequest(c, true)
	if !ok {
		return
	}

	h.bulkRunMu.Lock()
	defer h.bulkRunMu.Unlock()

	if current, exists, err := h.currentBulkRun(c.Request.Context(), dataOpsBulkRunActionMP4); err != nil {
		logger.Printf("[DATA_OPS] bulk MP4 current run lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load current bulk mp4 run"})
		return
	} else if exists {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "bulk mp4 already running",
			"run_id": current.RunID,
			"status": current.Status,
		})
		return
	}

	rows, skippedAutoFailed, err := h.selectBulkMP4EpisodeRows(c.Request.Context(), q)
	if err != nil {
		logger.Printf("[DATA_OPS] bulk MP4 snapshot failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to select data operation episodes"})
		return
	}

	run, err := h.createBulkRun(c.Request.Context(), dataOpsBulkRunActionMP4, int64(len(rows)))
	if err != nil {
		logger.Printf("[DATA_OPS] bulk MP4 run create failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bulk mp4 run"})
		return
	}

	logger.Printf("[DATA_OPS] Bulk MP4 accepted: run_id=%s total=%d auto_failed=%d", run.RunID, len(rows), skippedAutoFailed)
	if len(rows) > 0 {
		go h.runBulkEpisodeMP4(run.RunID, rows)
	}

	c.JSON(http.StatusAccepted, DataOpsBulkEpisodeQAActionResponse{
		Run:     run,
		Message: fmt.Sprintf("%d episodes accepted for bulk MP4 export", len(rows)),
	})
}

// DownloadBulkMP4 returns the generated ZIP for a completed bulk MP4 run.
func (h *DataOpsHandler) DownloadBulkMP4(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	runID := strings.TrimSpace(c.Param("run_id"))
	run, err := h.loadBulkRun(c.Request.Context(), runID)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "bulk run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bulk run"})
		return
	}
	if run.Action != dataOpsBulkRunActionMP4 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bulk run is not an mp4 export"})
		return
	}
	if run.Status != dataOpsBulkRunStatusCompleted {
		c.JSON(http.StatusConflict, gin.H{"error": "bulk mp4 export is not completed"})
		return
	}
	zipPath := h.bulkMP4ZipPath(runID)
	if _, err := os.Stat(zipPath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "bulk mp4 zip not found"})
		return
	}
	c.FileAttachment(zipPath, runID+".zip")
}

func (h *DataOpsHandler) ensureBulkMP4Configured(c *gin.Context) bool {
	if h == nil || h.qa == nil || (h.qa.s3 == nil && h.qa.tos == nil) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "storage service is not configured"})
		return false
	}
	if _, err := os.Stat(dataOpsBulkMP4Script); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mcap to mp4 script is not available"})
		return false
	}
	return true
}

func (h *DataOpsHandler) previewBulkEpisodeMP4(ctx context.Context, q dataOpsEpisodeQuery) (DataOpsBulkEpisodePreviewResponse, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)
	query := dataOpsBulkMP4PreviewSQL(fromSQL, where)

	var row dataOpsBulkMP4PreviewRow
	if err := h.db.GetContext(ctx, &row, query, args...); err != nil {
		return DataOpsBulkEpisodePreviewResponse{}, err
	}

	matched := int(row.MatchedCount)
	eligible := int(row.EligibleCount)
	autoFailed := int(row.AutoFailed)
	breakdown := []DataOpsBulkSkippedBreakdownItem{}
	if autoFailed > 0 {
		breakdown = append(breakdown, DataOpsBulkSkippedBreakdownItem{Reason: "auto_qa_failed", Count: autoFailed})
	}

	return DataOpsBulkEpisodePreviewResponse{
		Status:           "preview",
		Action:           dataOpsBulkRunActionMP4,
		MatchedCount:     matched,
		EligibleCount:    eligible,
		SkippedCount:     matched - eligible,
		SkippedBreakdown: breakdown,
		Warnings:         []string{},
	}, nil
}

func dataOpsBulkMP4PreviewSQL(fromSQL string, where string) string {
	return `
		SELECT
			COUNT(1) AS matched_count,
			COALESCE(SUM(CASE WHEN COALESCE(e.qa_status, '') = 'failed' THEN 1 ELSE 0 END), 0) AS auto_failed_count,
			COALESCE(SUM(CASE WHEN COALESCE(e.qa_status, '') <> 'failed' THEN 1 ELSE 0 END), 0) AS eligible_count
	` + fromSQL + where
}

func (h *DataOpsHandler) selectBulkMP4EpisodeRows(ctx context.Context, q dataOpsEpisodeQuery) ([]dataOpsBulkMP4EpisodeRow, int64, error) {
	fromSQL := dataOpsEpisodeBaseFromSQL()
	where, args := buildDataOpsEpisodeWhere(q)

	var skippedAutoFailed int64
	countQuery := `
		SELECT COALESCE(SUM(CASE WHEN COALESCE(e.qa_status, '') = 'failed' THEN 1 ELSE 0 END), 0)
	` + fromSQL + where
	if err := h.db.GetContext(ctx, &skippedAutoFailed, countQuery, args...); err != nil {
		return nil, 0, err
	}

	query := `
		SELECT e.id, e.episode_id, e.mcap_path, COALESCE(e.qa_status, '') AS qa_status
	` + fromSQL + where + `
		AND COALESCE(e.qa_status, '') <> 'failed'
		ORDER BY e.created_at DESC, e.id DESC
	`
	rows := []dataOpsBulkMP4EpisodeRow{}
	if err := h.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, 0, err
	}
	return rows, skippedAutoFailed, nil
}

func (h *DataOpsHandler) runBulkEpisodeMP4(runID string, rows []dataOpsBulkMP4EpisodeRow) {
	if _, err := h.markBulkRunRunning(context.Background(), runID); err != nil {
		logger.Printf("[DATA_OPS] Bulk MP4 failed to start: run_id=%s, err=%v", runID, err)
		return
	}

	workDir, err := os.MkdirTemp("", "keystone-bulk-mp4-*")
	if err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	mp4Dir := filepath.Join(workDir, "mp4")
	if err := os.MkdirAll(mp4Dir, 0o755); err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}

	zipPath := h.bulkMP4ZipPath(runID)
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}
	_ = os.Remove(zipPath)

	zipTempPath := zipPath + ".tmp"
	_ = os.Remove(zipTempPath)
	zipFile, err := os.Create(zipTempPath)
	if err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}
	zipWriter := zip.NewWriter(zipFile)
	zipClosed := false
	closeZip := func() error {
		if zipClosed {
			return nil
		}
		zipClosed = true
		if err := zipWriter.Close(); err != nil {
			_ = zipFile.Close()
			return err
		}
		return zipFile.Close()
	}
	defer func() {
		if !zipClosed {
			_ = zipWriter.Close()
			_ = zipFile.Close()
		}
		_ = os.Remove(zipTempPath)
	}()

	mp4Count := 0
	archiveNames := map[string]struct{}{}
	var lastProgressPublishedAt time.Time
	for _, row := range rows {
		mp4Path, cleanup, err := h.convertEpisodeMP4(context.Background(), row, workDir, mp4Dir)
		outcome := dataOpsBulkQAEpisodePassed
		if err != nil {
			logger.Printf("[DATA_OPS] Bulk MP4 episode failed: run_id=%s episode=%d err=%v", runID, row.ID, err)
			outcome = dataOpsBulkQAEpisodeProcessingFailed
		} else if err := addFileToZip(zipWriter, mp4Path, uniqueBulkMP4ArchiveName(row, archiveNames)); err != nil {
			logger.Printf("[DATA_OPS] Bulk MP4 zip append failed: run_id=%s episode=%d err=%v", runID, row.ID, err)
			outcome = dataOpsBulkQAEpisodeProcessingFailed
		} else {
			mp4Count++
		}
		if cleanup != nil {
			cleanup()
			cleanup = nil
		}
		run, err := h.incrementBulkQARunCounts(context.Background(), runID, outcome)
		if err != nil {
			logger.Printf("[DATA_OPS] Bulk MP4 progress update failed: run_id=%s episode=%d err=%v", runID, row.ID, err)
			continue
		}
		now := time.Now()
		if lastProgressPublishedAt.IsZero() || now.Sub(lastProgressPublishedAt) >= 500*time.Millisecond {
			h.publishBulkRunEvent("bulk_run_progress", run)
			lastProgressPublishedAt = now
		}
	}

	if mp4Count == 0 {
		finalRun, err := h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, "no mp4 files generated")
		if err == nil {
			h.publishBulkRunEvent("bulk_run_failed", finalRun)
		}
		return
	}
	if err := closeZip(); err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}
	if err := os.Rename(zipTempPath, zipPath); err != nil {
		_, _ = h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusFailed, err.Error())
		return
	}

	finalRun, err := h.markBulkRunTerminal(context.Background(), runID, dataOpsBulkRunStatusCompleted, "")
	if err != nil {
		logger.Printf("[DATA_OPS] Bulk MP4 completion update failed: run_id=%s err=%v", runID, err)
		return
	}
	logger.Printf(
		"[DATA_OPS] Bulk MP4 completed: run_id=%s total=%d processed=%d passed=%d auto_failed=%d processing_failed=%d zip=%s",
		runID,
		len(rows),
		finalRun.ProcessedCount,
		finalRun.PassedCount,
		finalRun.QAFailedCount,
		finalRun.ProcessingFailedCount,
		zipPath,
	)
}

// DownloadEpisodeMP4 converts one episode MCAP and returns the generated MP4.
func (h *DataOpsHandler) DownloadEpisodeMP4(c *gin.Context) {
	if !h.ensureDataOpsDatabase(c) {
		return
	}
	if !h.ensureBulkMP4Configured(c) {
		return
	}

	episodeID, ok := parseEpisodeIDParam(c)
	if !ok {
		return
	}

	var row dataOpsBulkMP4EpisodeRow
	err := h.db.GetContext(c.Request.Context(), &row, `
		SELECT e.id, e.episode_id, e.mcap_path, COALESCE(e.qa_status, '') AS qa_status
		FROM episodes e
		WHERE e.id = ? AND e.deleted_at IS NULL
		LIMIT 1
	`, episodeID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "episode not found"})
		return
	}
	if err != nil {
		logger.Printf("[DATA_OPS] MP4 episode lookup failed: episode=%d err=%v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load episode"})
		return
	}
	if row.QAStatus == qaStatusFailed {
		c.JSON(http.StatusConflict, gin.H{"error": "episode mp4 is blocked by failed qa status"})
		return
	}

	workDir, err := os.MkdirTemp("", "keystone-episode-mp4-*")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create mp4 workspace"})
		return
	}
	defer os.RemoveAll(workDir)

	mp4Path, cleanup, err := h.convertEpisodeMP4(c.Request.Context(), row, workDir, filepath.Join(workDir, "mp4"))
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		logger.Printf("[DATA_OPS] MP4 episode conversion failed: episode=%d err=%v", episodeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate mp4"})
		return
	}
	c.FileAttachment(mp4Path, bulkMP4EpisodeFileName(row))
}

func (h *DataOpsHandler) convertEpisodeMP4(ctx context.Context, row dataOpsBulkMP4EpisodeRow, workDir string, mp4Dir string) (string, func(), error) {
	bucket, objectName, ok := resolveEpisodeMcapLocation(h.qa.bucket, row.McapPath)
	if !ok {
		return "", nil, fmt.Errorf("invalid mcap_path")
	}

	safeName := safeBulkMP4FileName(row.EpisodeID, row.ID)
	inputPath := filepath.Join(workDir, safeName+".mcap")
	episodeOutputDir := filepath.Join(mp4Dir, safeName)
	cleanup := func() {
		_ = os.Remove(inputPath)
		_ = os.RemoveAll(episodeOutputDir)
	}
	if err := h.downloadBulkMP4Object(ctx, bucket, objectName, inputPath); err != nil {
		return "", cleanup, err
	}

	if err := os.MkdirAll(episodeOutputDir, 0o755); err != nil {
		return "", cleanup, err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, dataOpsBulkMP4Timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "python3", dataOpsBulkMP4Script, inputPath, "--output", episodeOutputDir, "--workers", "1", "--force")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", cleanup, fmt.Errorf("mcap_to_mp4 failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	mp4Path, err := findBulkMP4Output(episodeOutputDir, inputPath)
	if err != nil {
		return "", cleanup, fmt.Errorf("mp4 output not found: %w", err)
	}
	return mp4Path, cleanup, nil
}

func (h *DataOpsHandler) downloadBulkMP4Object(ctx context.Context, bucket string, objectName string, dst string) error {
	obj, err := h.openBulkMP4Object(ctx, bucket, objectName)
	if err != nil {
		return fmt.Errorf("get mcap object: %w", err)
	}
	defer obj.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, obj); err != nil {
		return fmt.Errorf("write mcap temp file: %w", err)
	}
	return nil
}

func (h *DataOpsHandler) openBulkMP4Object(ctx context.Context, bucket string, objectName string) (io.ReadCloser, error) {
	if h == nil || h.qa == nil {
		return nil, fmt.Errorf("data ops qa handler is not configured")
	}
	if h.qa.tos != nil {
		return h.qa.tos.OpenObject(ctx, bucket, objectName)
	}
	if h.qa.s3 == nil {
		return nil, fmt.Errorf("object storage client is not configured")
	}
	return h.qa.s3.GetObject(ctx, bucket, objectName, minio.GetObjectOptions{})
}

func findBulkMP4Output(outputDir string, inputPath string) (string, error) {
	expected := filepath.Join(outputDir, strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))+".mp4")
	if _, err := os.Stat(expected); err == nil {
		return expected, nil
	}

	var matches []string
	if err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".mp4") {
			return nil
		}
		matches = append(matches, path)
		return nil
	}); err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("found %d mp4 files under %s", len(matches), outputDir)
	}
	return matches[0], nil
}

func (h *DataOpsHandler) bulkMP4DownloadURL(runID string) string {
	if strings.TrimSpace(runID) == "" {
		return ""
	}
	return "/api/v1/data-ops/bulk-runs/" + runID + "/download"
}

func (h *DataOpsHandler) bulkMP4ZipPath(runID string) string {
	return filepath.Join(os.TempDir(), "keystone-data-ops-bulk-mp4", runID+".zip")
}

func safeBulkMP4FileName(episodeID string, id int64) string {
	name := strings.TrimSpace(episodeID)
	if name == "" {
		name = fmt.Sprintf("episode_%d", id)
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	return fmt.Sprintf("%s_%d", replacer.Replace(name), id)
}

func bulkMP4EpisodeFileName(row dataOpsBulkMP4EpisodeRow) string {
	name := sanitizeBulkMP4Name(row.EpisodeID)
	if name == "" {
		name = fmt.Sprintf("episode_%d", row.ID)
	}
	return name + ".mp4"
}

func uniqueBulkMP4ArchiveName(row dataOpsBulkMP4EpisodeRow, used map[string]struct{}) string {
	name := bulkMP4EpisodeFileName(row)
	if _, exists := used[name]; !exists {
		used[name] = struct{}{}
		return name
	}

	base := strings.TrimSuffix(name, filepath.Ext(name))
	ext := filepath.Ext(name)
	name = fmt.Sprintf("%s_%d%s", base, row.ID, ext)
	for i := 2; ; i++ {
		if _, exists := used[name]; !exists {
			used[name] = struct{}{}
			return name
		}
		name = fmt.Sprintf("%s_%d_%d%s", base, row.ID, i, ext)
	}
}

func sanitizeBulkMP4Name(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	return replacer.Replace(strings.TrimSpace(name))
}

func addFileToZip(zw *zip.Writer, filePath string, name string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = name
	header.Method = zip.Deflate

	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}
