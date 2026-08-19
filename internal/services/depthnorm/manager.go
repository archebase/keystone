// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package depthnorm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/logger"
	"archebase.com/keystone-edge/internal/storage/s3"
	"github.com/jmoiron/sqlx"
	"golang.org/x/sys/unix"
)

const (
	// Kind identifies local depth normalization derivatives.
	Kind = "depth_normalization"
	// DeviceTypeZJWA1D is the device family requiring this processing.
	DeviceTypeZJWA1D = "ZJ-WA1-D"

	statusQueued     = "queued"
	statusRunning    = "running"
	statusVerifying  = "verifying"
	statusSucceeded  = "succeeded"
	statusFailed     = "failed"
	statusQANotStart = "not_started"
	statusQAApproved = "approved"
)

var (
	// ErrDisabled indicates local depth normalization is unavailable.
	ErrDisabled = errors.New("depth normalization is disabled")
	// ErrEpisodeNotFound indicates the source Episode does not exist.
	ErrEpisodeNotFound = errors.New("episode not found")
	// ErrAlreadyDerived indicates a successful derivative already exists.
	ErrAlreadyDerived = errors.New("episode is already depth normalized")
	// ErrProcessingActive indicates processing is currently active.
	ErrProcessingActive = errors.New("depth normalization processing is active")
	// ErrCloudSourceLocked indicates the Episode committed to another source.
	ErrCloudSourceLocked = errors.New("episode cloud source is locked")
)

// Derivative is the durable projection returned to callers.
type Derivative struct {
	ID               int64          `db:"id" json:"id"`
	EpisodeID        int64          `db:"episode_id" json:"episode_id"`
	Generation       int            `db:"generation" json:"generation"`
	ProcessingStatus string         `db:"processing_status" json:"processing_status"`
	QAStatus         string         `db:"qa_status" json:"qa_status"`
	McapPath         sql.NullString `db:"mcap_path" json:"mcap_path"`
	Checksum         sql.NullString `db:"checksum" json:"checksum"`
	FileSizeBytes    sql.NullInt64  `db:"file_size_bytes" json:"file_size_bytes"`
	ProcessingError  sql.NullString `db:"processing_error" json:"processing_error"`
}

// Config controls the edge-local derivative executor.
type Config struct {
	Enabled      bool
	Script       string
	Timeout      time.Duration
	MinFreeDisk  int64
	OutputPrefix string
	PollInterval time.Duration
}

// Manager owns the edge-local Python derivative lifecycle.
type Manager struct {
	db         *sqlx.DB
	s3         *s3.Client
	bucket     string
	cfg        Config
	cancel     context.CancelFunc
	done       chan struct{}
	stopCalled bool
}

// NewManager constructs the edge-local derivative manager.
func NewManager(db *sqlx.DB, client *s3.Client, bucket string, cfg Config) *Manager {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Minute
	}
	if cfg.MinFreeDisk <= 0 {
		cfg.MinFreeDisk = 8
	}
	if cfg.OutputPrefix == "" {
		cfg.OutputPrefix = "depth-normalized"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	return &Manager{db: db, s3: client, bucket: bucket, cfg: cfg}
}

type episodeRow struct {
	ID             int64          `db:"id"`
	EpisodeID      string         `db:"episode_id"`
	McapPath       string         `db:"mcap_path"`
	StorageBackend sql.NullString `db:"storage_backend"`
	Checksum       sql.NullString `db:"checksum"`
	FileSizeBytes  sql.NullInt64  `db:"file_size_bytes"`
	CloudPublish   sql.NullString `db:"cloud_publish_source"`
	CloudSynced    bool           `db:"cloud_synced"`
	Metadata       sql.NullString `db:"metadata"`
	DeviceType     sql.NullString `db:"device_type"`
}

func (m *Manager) loadEpisode(ctx context.Context, q sqlx.QueryerContext, id int64) (episodeRow, error) {
	var row episodeRow
	err := sqlx.GetContext(ctx, q, &row, `
		SELECT e.id, e.episode_id, COALESCE(e.mcap_path, '') AS mcap_path, e.storage_backend,
		       e.checksum, e.file_size_bytes, e.cloud_publish_source, e.cloud_synced,
		       e.metadata, r.device_type AS device_type
		FROM episodes e
		LEFT JOIN tasks t ON t.id=e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws ON ws.id=COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id=ws.robot_id AND r.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL
	`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, ErrEpisodeNotFound
	}
	return row, err
}

func objectLocation(bucket, storedPath string) (string, bool) {
	key := strings.TrimLeft(strings.TrimSpace(storedPath), "/")
	if strings.HasPrefix(key, bucket+"/") {
		key = strings.TrimPrefix(key, bucket+"/")
	}
	return key, key != ""
}

// Start admits one Episode into the local processing queue.
func (m *Manager) Start(ctx context.Context, episodeID int64, actor string) (Derivative, bool, error) {
	if m == nil || m.db == nil {
		return Derivative{}, false, ErrDisabled
	}
	if !m.cfg.Enabled || m.s3 == nil || strings.TrimSpace(m.bucket) == "" {
		return Derivative{}, false, ErrDisabled
	}
	if episodeID <= 0 {
		return Derivative{}, false, fmt.Errorf("invalid episode id")
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Derivative{}, false, fmt.Errorf("begin depth normalization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	episode, err := m.loadEpisode(ctx, tx, episodeID)
	if err != nil {
		return Derivative{}, false, err
	}
	if !strings.EqualFold(strings.TrimSpace(episode.DeviceType.String), DeviceTypeZJWA1D) {
		return Derivative{}, false, fmt.Errorf("episode %d device type is not %s", episodeID, DeviceTypeZJWA1D)
	}
	if episode.CloudSynced {
		return Derivative{}, false, ErrCloudSourceLocked
	}
	if claimed := strings.TrimSpace(episode.CloudPublish.String); claimed != "" && claimed != "depth_normalization" {
		return Derivative{}, false, fmt.Errorf("%w: source is %q", ErrCloudSourceLocked, claimed)
	}
	var syncEvidence int
	if err := tx.GetContext(ctx, &syncEvidence, `SELECT COUNT(*) FROM sync_logs WHERE episode_id = ?`, episodeID); err != nil {
		return Derivative{}, false, fmt.Errorf("check sync evidence: %w", err)
	}
	if syncEvidence > 0 && strings.TrimSpace(episode.CloudPublish.String) != "depth_normalization" {
		return Derivative{}, false, ErrCloudSourceLocked
	}

	objectKey, ok := objectLocation(m.bucket, episode.McapPath)
	if !ok {
		return Derivative{}, false, fmt.Errorf("episode %d has invalid mcap object location", episodeID)
	}
	uri := "minio://" + m.bucket + "/" + objectKey
	now := time.Now().UTC()
	var derivative Derivative
	err = tx.GetContext(ctx, &derivative, `
		SELECT id, episode_id, generation, processing_status, qa_status, mcap_path,
		       checksum, file_size_bytes, processing_error
		FROM episode_derivatives WHERE episode_id = ? AND kind = ?
	`, episodeID, Kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		result, insertErr := tx.ExecContext(ctx, `
			INSERT INTO episode_derivatives (
				episode_id, kind, generation, source_uri, source_checksum, source_size_bytes,
				processing_status, qa_status, created_by, updated_by, created_at, updated_at
			) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, episodeID, Kind, uri, strings.TrimSpace(episode.Checksum.String), episode.FileSizeBytes.Int64,
			statusQueued, statusQANotStart, actor, actor, now, now)
		if insertErr != nil {
			return Derivative{}, false, fmt.Errorf("insert depth normalization derivative: %w", insertErr)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return Derivative{}, false, fmt.Errorf("read depth normalization id: %w", err)
		}
		derivative = Derivative{ID: id, EpisodeID: episodeID, Generation: 1, ProcessingStatus: statusQueued, QAStatus: statusQANotStart}
	case err != nil:
		return Derivative{}, false, fmt.Errorf("load depth normalization derivative: %w", err)
	case derivative.ProcessingStatus == statusSucceeded:
		return derivative, false, ErrAlreadyDerived
	case derivative.ProcessingStatus == statusQueued || derivative.ProcessingStatus == statusRunning || derivative.ProcessingStatus == statusVerifying:
		return derivative, false, ErrProcessingActive
	default:
		generation := derivative.Generation + 1
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE episode_derivatives
			SET generation=?, source_uri=?, source_checksum=?, source_size_bytes=?,
			    processing_status=?, qa_status=?, mcap_path=NULL, checksum=NULL,
			    file_size_bytes=NULL, processing_error=NULL, processing_result=NULL,
			    processing_started_at=NULL, processing_finished_at=NULL,
			    updated_by=?, updated_at=?
			WHERE id=? AND episode_id=? AND kind=? AND processing_status=?
		`, generation, uri, strings.TrimSpace(episode.Checksum.String), episode.FileSizeBytes.Int64,
			statusQueued, statusQANotStart, actor, now, derivative.ID, episodeID, Kind, derivative.ProcessingStatus)
		if updateErr != nil {
			return Derivative{}, false, fmt.Errorf("retry depth normalization: %w", updateErr)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return Derivative{}, false, fmt.Errorf("read depth normalization retry result: %w", err)
		}
		if rows != 1 {
			return Derivative{}, false, ErrProcessingActive
		}
		derivative.Generation = generation
		derivative.ProcessingStatus = statusQueued
		derivative.QAStatus = statusQANotStart
	}
	if err := tx.Commit(); err != nil {
		return Derivative{}, false, fmt.Errorf("commit depth normalization: %w", err)
	}
	return derivative, true, nil
}

// StartWorker starts the single-slot local executor loop.
func (m *Manager) StartWorker() error {
	if m == nil || m.db == nil || !m.cfg.Enabled {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.run(ctx)
	return nil
}

// StopWorker cancels the local executor and waits for the active task.
func (m *Manager) StopWorker(ctx context.Context) error {
	if m == nil || m.cancel == nil {
		return nil
	}
	if !m.stopCalled {
		m.stopCalled = true
		m.cancel()
	}
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	m.recoverInterrupted(context.Background())
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		m.processNext(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) recoverInterrupted(ctx context.Context) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status=?, processing_error=COALESCE(processing_error, 'requeued after process restart'),
		    updated_at=?
		WHERE kind=? AND processing_status IN (?, ?)
	`, statusQueued, time.Now().UTC(), Kind, statusRunning, statusVerifying); err != nil {
		logger.Printf("[DEPTH-NORM] interrupted task recovery failed: %v", err)
	}
}

type taskRow struct {
	DerivativeID   int64          `db:"derivative_id"`
	EpisodeID      int64          `db:"episode_id"`
	EpisodeUUID    string         `db:"episode_uuid"`
	Generation     int            `db:"generation"`
	SourceURI      sql.NullString `db:"source_uri"`
	SourceChecksum sql.NullString `db:"source_checksum"`
	SourceSize     sql.NullInt64  `db:"source_size_bytes"`
	McapPath       string         `db:"mcap_path"`
	Metadata       sql.NullString `db:"metadata"`
}

func (m *Manager) processNext(ctx context.Context) {
	var task taskRow
	err := m.db.GetContext(ctx, &task, `
		SELECT ed.id AS derivative_id, ed.episode_id, e.episode_id AS episode_uuid,
		       ed.generation, ed.source_uri, ed.source_checksum, ed.source_size_bytes,
		       COALESCE(e.mcap_path, '') AS mcap_path, e.metadata
		FROM episode_derivatives ed
		INNER JOIN episodes e ON e.id=ed.episode_id AND e.deleted_at IS NULL
		WHERE ed.kind=? AND ed.processing_status=?
		ORDER BY ed.id LIMIT 1
	`, Kind, statusQueued)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		logger.Printf("[DEPTH-NORM] task lookup failed: %v", err)
		return
	}
	claim, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status=?, processing_started_at=?, processing_error=NULL, updated_at=?
		WHERE id=? AND kind=? AND processing_status=?
	`, statusRunning, time.Now().UTC(), time.Now().UTC(), task.DerivativeID, Kind, statusQueued)
	if err != nil {
		logger.Printf("[DEPTH-NORM] task claim failed: %v", err)
		return
	}
	rows, err := claim.RowsAffected()
	if err != nil || rows != 1 {
		return
	}
	if err := m.processTask(ctx, task); err != nil {
		m.markFailed(context.Background(), task.DerivativeID, err)
		logger.Printf("[DEPTH-NORM] episode=%d generation=%d failed: %v", task.EpisodeID, task.Generation, err)
	}
}

func (m *Manager) markFailed(ctx context.Context, derivativeID int64, cause error) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status=?, processing_error=?, processing_finished_at=?, updated_at=?
		WHERE id=? AND kind=?
	`, statusFailed, cause.Error(), time.Now().UTC(), time.Now().UTC(), derivativeID, Kind); err != nil {
		logger.Printf("[DEPTH-NORM] failed to persist failure derivative=%d: %v", derivativeID, err)
	}
}

func hasFreeDisk(path string, minGB int64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("check free disk: %w", err)
	}
	blockSize := uint64(stat.Bsize) // #nosec G115 -- Statfs reports a positive block size.
	free := stat.Bavail * blockSize
	required := uint64(minGB) * 1024 * 1024 * 1024 // #nosec G115 -- constructor validates a positive minimum.
	if free < required {
		return fmt.Errorf("insufficient free disk: have %d bytes, need %d bytes", free, required)
	}
	return nil
}

func runScript(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// #nosec G204 -- the executable is fixed; args are generated by Keystone.
	cmd := exec.CommandContext(execCtx, "python3", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		tail := output
		if len(tail) > 64*1024 {
			tail = tail[len(tail)-64*1024:]
		}
		return nil, fmt.Errorf("python script failed: %w: %s", err, strings.TrimSpace(string(tail)))
	}
	return output, nil
}

func decodeJSON(output []byte, target any) error {
	start := strings.Index(strings.TrimSpace(string(output)), "{")
	end := strings.LastIndex(strings.TrimSpace(string(output)), "}")
	if start < 0 || end < start {
		return fmt.Errorf("script did not return JSON: %s", strings.TrimSpace(string(output)))
	}
	if err := json.Unmarshal(output[start:end+1], target); err != nil {
		return fmt.Errorf("decode script JSON: %w", err)
	}
	return nil
}

func mergeNotRequiredMetadata(raw sql.NullString, reason string) (string, error) {
	value := map[string]any{}
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		if err := json.Unmarshal([]byte(raw.String), &value); err != nil {
			return "", fmt.Errorf("decode episode metadata: %w", err)
		}
	}
	value["depth_normalization"] = map[string]any{
		"required":    false,
		"reason":      reason,
		"checked_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"output_only": true,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (m *Manager) processTask(ctx context.Context, task taskRow) error {
	workDir, err := os.MkdirTemp("", "keystone-depth-norm-*")
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if err := hasFreeDisk(workDir, m.cfg.MinFreeDisk); err != nil {
		return err
	}

	sourceKey, ok := objectLocation(m.bucket, task.McapPath)
	if !ok {
		return fmt.Errorf("invalid source object location")
	}
	sourcePath := filepath.Join(workDir, "input.mcap")
	if err := m.download(ctx, sourceKey, sourcePath); err != nil {
		return err
	}
	sourceChecksum, sourceSize, err := fileHash(sourcePath)
	if err != nil {
		return err
	}
	if task.SourceChecksum.Valid && !strings.EqualFold(strings.TrimSpace(task.SourceChecksum.String), sourceChecksum) {
		return fmt.Errorf("source checksum changed: got %s want %s", sourceChecksum, task.SourceChecksum.String)
	}
	if task.SourceSize.Valid && task.SourceSize.Int64 > 0 && task.SourceSize.Int64 != sourceSize {
		return fmt.Errorf("source size changed: got %d want %d", sourceSize, task.SourceSize.Int64)
	}

	inspectOutput, err := runScript(ctx, m.cfg.Timeout, m.cfg.Script, "--inspect", sourcePath)
	if err != nil {
		return err
	}
	var inspection struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(inspectOutput, &inspection); err != nil {
		return err
	}
	if inspection.Status == "already_target" {
		metadata, mergeErr := mergeNotRequiredMetadata(task.Metadata, "already_target")
		if mergeErr != nil {
			return mergeErr
		}
		if _, err := m.db.ExecContext(ctx, `UPDATE episodes SET metadata=?, updated_at=? WHERE id=?`,
			metadata, time.Now().UTC(), task.EpisodeID); err != nil {
			return fmt.Errorf("mark depth normalization not required: %w", err)
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE episode_derivatives
			SET processing_status=?, processing_error='input already uses compressedDepth',
			    processing_finished_at=?, updated_at=?
			WHERE id=? AND kind=? AND processing_status=?
		`, statusFailed, time.Now().UTC(), time.Now().UTC(), task.DerivativeID, Kind, statusRunning); err != nil {
			return fmt.Errorf("record skipped derivative: %w", err)
		}
		return nil
	}
	if inspection.Status != "requires_normalization" && inspection.Status != "requires_chest_normalization" {
		return fmt.Errorf("unsupported depth format %q", inspection.Status)
	}

	if _, err := m.db.ExecContext(ctx, `UPDATE episode_derivatives SET processing_status=?, updated_at=? WHERE id=? AND kind=?`,
		statusVerifying, time.Now().UTC(), task.DerivativeID, Kind); err != nil {
		return fmt.Errorf("enter verification: %w", err)
	}

	outputPath := filepath.Join(workDir, "output.mcap")
	convertOutput, err := runScript(ctx, m.cfg.Timeout, m.cfg.Script, sourcePath, outputPath)
	if err != nil {
		return err
	}
	var result struct {
		Verification struct {
			Valid bool `json:"valid"`
		} `json:"verification"`
	}
	if err := decodeJSON(convertOutput, &result); err != nil {
		return err
	}
	if !result.Verification.Valid {
		return fmt.Errorf("output verification was invalid")
	}
	checksum, size, err := fileHash(outputPath)
	if err != nil {
		return err
	}
	objectKey := fmt.Sprintf("%s/%s/generation-%d/%s", strings.Trim(m.cfg.OutputPrefix, "/"), task.EpisodeUUID, task.Generation, filepath.Base(sourcePath))
	if err := m.upload(ctx, objectKey, outputPath); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE episode_derivatives
		SET processing_status=?, qa_status=?, mcap_path=?, checksum=?, file_size_bytes=?,
		    processing_finished_at=?, updated_at=?
		WHERE id=? AND kind=? AND processing_status=?
	`, statusSucceeded, statusQAApproved, objectKey, checksum, size, now, now,
		task.DerivativeID, Kind, statusVerifying); err != nil {
		return fmt.Errorf("commit derivative: %w", err)
	}
	logger.Printf("[DEPTH-NORM] episode=%d generation=%d succeeded bytes=%d", task.EpisodeID, task.Generation, size)
	return nil
}
