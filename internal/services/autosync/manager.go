// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

// Package autosync owns the durable automatic QA-to-cloud orchestration policy.
package autosync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"archebase.com/keystone-edge/internal/services/depthnorm"
	"archebase.com/keystone-edge/internal/services/stereosplit"
)

const (
	defaultPollInterval = 5 * time.Second

	// DeviceTypeEgoPortalStereo requires QA, stereo split, derivative QA, then cloud sync.
	DeviceTypeEgoPortalStereo = "Ego Portal Stereo"
	// DeviceTypeEgoPortalLite requires QA followed by original Episode cloud sync.
	DeviceTypeEgoPortalLite = "Ego Portal Lite"
	// DeviceTypeZJWA1D requires local depth normalization before cloud sync.
	DeviceTypeZJWA1D = depthnorm.DeviceTypeZJWA1D
)

var (
	// ErrConfigChanged indicates that an administrator updated a stale revision.
	ErrConfigChanged = errors.New("auto sync config changed")
)

// Config is one immutable automatic-sync setting revision.
type Config struct {
	ID              int64     `db:"id" json:"revision_id"`
	Enabled         bool      `db:"enabled" json:"enabled"`
	PreviousEnabled *bool     `db:"previous_enabled" json:"previous_enabled,omitempty"`
	CreatedBy       string    `db:"created_by_value" json:"created_by,omitempty"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

// StereoSplitter admits one Episode into the existing durable derivative queue.
type StereoSplitter interface {
	Start(ctx context.Context, episodeID int64, actor string) (stereosplit.Derivative, bool, error)
}

// CloudSyncEnqueuer persists source-specific work in the existing Sync Worker.
type CloudSyncEnqueuer interface {
	EnqueueOriginalAutomatic(ctx context.Context, episodeID int64) error
	EnqueueStereoSplitManual(ctx context.Context, episodeID int64) error
	EnqueueDepthNormalizationAutomatic(ctx context.Context, episodeID int64) error
}

// DepthNormalizer admits one Episode into the local derivative queue.
type DepthNormalizer interface {
	Start(ctx context.Context, episodeID int64, actor string) (depthnorm.Derivative, bool, error)
}

// QAEnqueuer restores captured Episodes to the existing automatic QA queue.
type QAEnqueuer interface {
	EnqueueEpisode(episodeID int64)
}

// Manager exposes settings and the automatic processing lifecycle behind one interface.
type Manager struct {
	db           *sqlx.DB
	stereo       StereoSplitter
	depthNorm    DepthNormalizer
	cloud        CloudSyncEnqueuer
	qa           QAEnqueuer
	pollInterval time.Duration

	configMu sync.RWMutex
	wake     chan struct{}
	runnerMu sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

// SetQAEnqueuer connects the existing automatic QA queue during server initialization.
func (m *Manager) SetQAEnqueuer(qa QAEnqueuer) {
	if m == nil {
		return
	}
	m.qa = qa
}

// NewManager constructs the automatic-sync module.
func NewManager(db *sqlx.DB, stereo StereoSplitter, cloud CloudSyncEnqueuer, pollInterval time.Duration, normalizers ...DepthNormalizer) *Manager {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	return &Manager{
		db:           db,
		stereo:       stereo,
		depthNorm:    firstDepthNormalizer(normalizers),
		cloud:        cloud,
		pollInterval: pollInterval,
		wake:         make(chan struct{}, 1),
	}
}

func firstDepthNormalizer(values []DepthNormalizer) DepthNormalizer {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

// CurrentConfig returns the effective automatic-sync setting revision.
func (m *Manager) CurrentConfig(ctx context.Context) (Config, error) {
	if m == nil || m.db == nil {
		return Config{}, fmt.Errorf("load auto sync config: database is not configured")
	}
	var config Config
	if err := m.db.GetContext(ctx, &config, autoSyncConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		return Config{}, fmt.Errorf("load current auto sync config: %w", err)
	}
	return config, nil
}

// CaptureEpisode records that a supported Episode was uploaded while automatic sync was enabled.
func (m *Manager) CaptureEpisode(ctx context.Context, episodeID int64) (bool, error) {
	if m == nil || m.db == nil {
		return false, fmt.Errorf("capture auto sync episode: database is not configured")
	}
	if episodeID <= 0 {
		return false, fmt.Errorf("capture auto sync episode: invalid episode id")
	}

	m.configMu.RLock()
	defer m.configMu.RUnlock()

	var upload struct {
		DeviceType      string `db:"device_type"`
		AutoSyncEnabled bool   `db:"auto_sync_enabled"`
	}
	if err := m.db.GetContext(ctx, &upload, `
		SELECT COALESCE(r.device_type, '') AS device_type,
		       COALESCE((
			SELECT ascfg.enabled
			FROM auto_sync_configs ascfg
			WHERE ascfg.created_at <= e.auto_sync_observed_at
			ORDER BY ascfg.created_at DESC, ascfg.id DESC
			LIMIT 1
		), FALSE) AS auto_sync_enabled
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws
			ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		LEFT JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE e.id = ? AND e.deleted_at IS NULL
		LIMIT 1
	`, episodeID); err != nil {
		return false, fmt.Errorf("resolve auto sync upload eligibility for episode %d: %w", episodeID, err)
	}
	if !upload.AutoSyncEnabled || !supportedDeviceType(upload.DeviceType) ||
		(strings.EqualFold(strings.TrimSpace(upload.DeviceType), DeviceTypeZJWA1D) && m.depthNorm == nil) {
		return false, nil
	}

	now := time.Now().UTC()
	result, err := m.db.ExecContext(ctx, `
		UPDATE episodes
		SET auto_sync_requested = TRUE,
		    auto_sync_device_type = ?,
		    auto_sync_requested_at = COALESCE(auto_sync_requested_at, ?)
		WHERE id = ? AND deleted_at IS NULL AND auto_sync_requested = FALSE
	`, upload.DeviceType, now, episodeID)
	if err != nil {
		return false, fmt.Errorf("capture auto sync episode %d: %w", episodeID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read captured auto sync episode %d: %w", episodeID, err)
	}
	if affected == 0 {
		return false, nil
	}
	m.wakeWorker()
	return true, nil
}

// ReconcileOnce advances at most one captured Episode through the automatic pipeline.
func (m *Manager) ReconcileOnce(ctx context.Context) (bool, error) {
	if m == nil || m.db == nil {
		return false, fmt.Errorf("reconcile auto sync: database is not configured")
	}
	if _, err := m.recoverMissedCapture(ctx); err != nil {
		return false, err
	}

	m.configMu.RLock()
	config, err := m.CurrentConfig(ctx)
	if err != nil {
		m.configMu.RUnlock()
		return false, err
	}
	if config.Enabled {
		worked, reconcileErr := m.reconcileDownstream(ctx)
		m.configMu.RUnlock()
		if worked || reconcileErr != nil {
			return worked, reconcileErr
		}
	} else {
		m.configMu.RUnlock()
	}
	return m.reconcilePendingQA(ctx)
}

func (m *Manager) recoverMissedCapture(ctx context.Context) (bool, error) {
	var candidate struct {
		ID         int64     `db:"id"`
		DeviceType string    `db:"device_type"`
		ObservedAt time.Time `db:"auto_sync_observed_at"`
	}
	deviceTypes := autoSyncDeviceTypeArgs(m.depthNorm != nil)
	deviceArgs := make([]any, 0, len(deviceTypes))
	for _, deviceType := range deviceTypes {
		deviceArgs = append(deviceArgs, deviceType)
	}
	recoverQuery := fmt.Sprintf(`
		SELECT e.id, r.device_type, e.auto_sync_observed_at
		FROM episodes e
		LEFT JOIN tasks t ON t.id = e.task_id AND t.deleted_at IS NULL
		LEFT JOIN workstations ws
			ON ws.id = COALESCE(e.workstation_id, t.workstation_id) AND ws.deleted_at IS NULL
		INNER JOIN robots r ON r.id = ws.robot_id AND r.deleted_at IS NULL
		WHERE e.auto_sync_requested = FALSE
		  AND e.cloud_synced = FALSE
		  AND e.cloud_publish_source IS NULL
		  AND e.deleted_at IS NULL
		  AND HEX(r.device_type) IN (%s)
		  AND e.auto_sync_observed_at >= (
			SELECT MIN(enabled_cfg.created_at)
			FROM auto_sync_configs enabled_cfg
			WHERE enabled_cfg.enabled = TRUE
		  )
		  AND NOT EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id)
		  AND (
			SELECT ascfg.enabled
			FROM auto_sync_configs ascfg
			WHERE ascfg.created_at <= e.auto_sync_observed_at
			ORDER BY ascfg.created_at DESC, ascfg.id DESC
			LIMIT 1
		  ) = TRUE
		ORDER BY e.auto_sync_observed_at ASC, e.id ASC
		LIMIT 1
	`, autoSyncDeviceTypeSQL(len(deviceTypes)))
	err := m.db.GetContext(ctx, &candidate, recoverQuery, deviceArgs...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select missed auto sync capture: %w", err)
	}
	result, err := m.db.ExecContext(ctx, `
		UPDATE episodes
		SET auto_sync_requested = TRUE,
		    auto_sync_device_type = ?,
		    auto_sync_requested_at = COALESCE(auto_sync_requested_at, ?)
		WHERE id = ? AND auto_sync_requested = FALSE AND deleted_at IS NULL
	`, candidate.DeviceType, candidate.ObservedAt, candidate.ID)
	if err != nil {
		return false, fmt.Errorf("recover missed auto sync capture for episode %d: %w", candidate.ID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read recovered auto sync capture for episode %d: %w", candidate.ID, err)
	}
	return affected > 0, nil
}

func (m *Manager) reconcileDownstream(ctx context.Context) (bool, error) {
	var episodeID int64
	err := m.db.GetContext(ctx, &episodeID, `
		SELECT e.id
		FROM episodes e
		WHERE e.auto_sync_requested = TRUE
		  AND e.auto_sync_device_type = ?
		  AND e.qa_status = 'approved'
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		  AND (e.cloud_publish_source IS NULL OR e.cloud_publish_source = 'original')
		  AND NOT EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id)
		ORDER BY e.auto_sync_requested_at ASC, e.id ASC
		LIMIT 1
	`, DeviceTypeEgoPortalLite)
	if err == nil {
		if m.cloud == nil {
			return false, fmt.Errorf("automatic cloud sync is not configured")
		}
		if err := m.cloud.EnqueueOriginalAutomatic(ctx, episodeID); err != nil {
			return false, fmt.Errorf("enqueue automatic original sync for episode %d: %w", episodeID, err)
		}
		m.wakeWorker()
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("select auto sync candidate: %w", err)
	}

	err = m.db.GetContext(ctx, &episodeID, `
		SELECT e.id
		FROM episodes e
		WHERE e.auto_sync_requested = TRUE
		  AND e.auto_sync_device_type = ?
		  AND e.qa_status = 'approved'
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		  AND (e.cloud_publish_source IS NULL OR e.cloud_publish_source = 'stereo_split')
		  AND EXISTS (
			SELECT 1
			FROM episode_derivatives ed
			WHERE ed.episode_id = e.id
			  AND ed.kind = 'stereo_split'
			  AND ed.processing_status = 'succeeded'
			  AND ed.qa_status = 'approved'
		  )
		  AND NOT EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id)
		ORDER BY e.auto_sync_requested_at ASC, e.id ASC
		LIMIT 1
	`, DeviceTypeEgoPortalStereo)
	if err == nil {
		if m.cloud == nil {
			return false, fmt.Errorf("automatic cloud sync is not configured")
		}
		if err := m.cloud.EnqueueStereoSplitManual(ctx, episodeID); err != nil {
			return false, fmt.Errorf("enqueue automatic stereo split sync for episode %d: %w", episodeID, err)
		}
		m.wakeWorker()
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("select approved automatic stereo derivative: %w", err)
	}

	if worked, err := m.reconcileZJWA1D(ctx); worked || err != nil {
		return worked, err
	}

	err = m.db.GetContext(ctx, &episodeID, `
		SELECT e.id
		FROM episodes e
		LEFT JOIN episode_derivatives ed
			ON ed.episode_id = e.id AND ed.kind = 'stereo_split'
		WHERE e.auto_sync_requested = TRUE
		  AND e.auto_sync_device_type = ?
		  AND e.qa_status = 'approved'
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		  AND (e.cloud_publish_source IS NULL OR e.cloud_publish_source = 'stereo_split')
		  AND ed.id IS NULL
		  AND NOT EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id = e.id)
		ORDER BY e.auto_sync_requested_at ASC, e.id ASC
		LIMIT 1
	`, DeviceTypeEgoPortalStereo)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select automatic stereo split candidate: %w", err)
	}
	if m.stereo == nil {
		return false, fmt.Errorf("automatic stereo split is not configured")
	}
	if _, _, err := m.stereo.Start(ctx, episodeID, "auto-sync"); err != nil && !errors.Is(err, stereosplit.ErrAlreadyDerived) {
		return false, fmt.Errorf("start automatic stereo split for episode %d: %w", episodeID, err)
	}
	m.wakeWorker()
	return true, nil
}

func zjwa1dMetadataNotRequired(raw sql.NullString) bool {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return false
	}
	var metadata struct {
		DepthNormalization struct {
			Required bool   `json:"required"`
			Reason   string `json:"reason"`
		} `json:"depth_normalization"`
	}
	if err := json.Unmarshal([]byte(raw.String), &metadata); err != nil {
		return false
	}
	if metadata.DepthNormalization.Required {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(metadata.DepthNormalization.Reason))
	return reason == "already_target" || reason == "already_compresseddepth"
}

func (m *Manager) reconcileZJWA1D(ctx context.Context) (bool, error) {
	var rows []struct {
		ID               int64          `db:"id"`
		CloudSynced      bool           `db:"cloud_synced"`
		CloudPublish     sql.NullString `db:"cloud_publish_source"`
		Metadata         sql.NullString `db:"metadata"`
		DerivativeID     sql.NullInt64  `db:"derivative_id"`
		ProcessingStatus sql.NullString `db:"processing_status"`
		QAStatus         sql.NullString `db:"derivative_qa_status"`
	}
	err := m.db.SelectContext(ctx, &rows, `
		SELECT e.id, e.cloud_synced, e.cloud_publish_source, e.metadata,
		       ed.id AS derivative_id, ed.processing_status,
		       ed.qa_status AS derivative_qa_status
		FROM episodes e
		LEFT JOIN episode_derivatives ed
		  ON ed.episode_id=e.id AND ed.kind='depth_normalization'
		WHERE e.auto_sync_requested=TRUE
		  AND e.auto_sync_device_type=?
		  AND e.qa_status='approved'
		  AND e.cloud_synced=FALSE
		  AND e.deleted_at IS NULL
		  AND (e.cloud_publish_source IS NULL OR e.cloud_publish_source='depth_normalization')
		  AND NOT EXISTS (SELECT 1 FROM sync_logs sl WHERE sl.episode_id=e.id)
		ORDER BY e.auto_sync_requested_at, e.id
		LIMIT 20
	`, DeviceTypeZJWA1D)
	if err != nil {
		return false, fmt.Errorf("select ZJ-WA1-D auto sync candidates: %w", err)
	}
	for _, row := range rows {
		if m.cloud == nil {
			return false, fmt.Errorf("automatic cloud sync is not configured")
		}
		if zjwa1dMetadataNotRequired(row.Metadata) {
			if err := m.cloud.EnqueueOriginalAutomatic(ctx, row.ID); err != nil {
				return false, fmt.Errorf("enqueue ZJ-WA1-D original sync for episode %d: %w", row.ID, err)
			}
			m.wakeWorker()
			return true, nil
		}
		if row.DerivativeID.Valid && row.ProcessingStatus.String == "succeeded" && row.QAStatus.String == "approved" {
			if err := m.cloud.EnqueueDepthNormalizationAutomatic(ctx, row.ID); err != nil {
				return false, fmt.Errorf("enqueue ZJ-WA1-D depth normalization sync for episode %d: %w", row.ID, err)
			}
			m.wakeWorker()
			return true, nil
		}
		if !row.DerivativeID.Valid {
			if m.depthNorm == nil {
				return false, fmt.Errorf("automatic depth normalization is not configured")
			}
			if _, _, err := m.depthNorm.Start(ctx, row.ID, "auto-sync"); err != nil && !errors.Is(err, depthnorm.ErrAlreadyDerived) {
				return false, fmt.Errorf("start automatic depth normalization for episode %d: %w", row.ID, err)
			}
			m.wakeWorker()
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) reconcilePendingQA(ctx context.Context) (bool, error) {
	var episodeID int64
	err := m.db.GetContext(ctx, &episodeID, `
		SELECT e.id
		FROM episodes e
		WHERE e.auto_sync_requested = TRUE
		  AND e.qa_status = 'pending_qa'
		  AND e.cloud_synced = FALSE
		  AND e.deleted_at IS NULL
		ORDER BY e.auto_sync_requested_at ASC, e.id ASC
		LIMIT 1
	`)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select pending automatic episode QA: %w", err)
	}
	if m.qa == nil {
		return false, fmt.Errorf("automatic episode QA is not configured")
	}
	m.qa.EnqueueEpisode(episodeID)
	return true, nil
}

// UpdateConfig atomically appends a new audited automatic-sync setting revision.
func (m *Manager) UpdateConfig(ctx context.Context, enabled bool, expectedRevisionID int64, actor string) (Config, error) {
	if m == nil || m.db == nil {
		return Config{}, fmt.Errorf("update auto sync config: database is not configured")
	}
	if expectedRevisionID <= 0 {
		return Config{}, fmt.Errorf("expected revision id must be positive")
	}

	m.configMu.Lock()
	defer m.configMu.Unlock()

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Config{}, fmt.Errorf("begin auto sync config update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lockClause := ""
	if m.db.DriverName() == "mysql" {
		lockClause = " FOR UPDATE"
	}
	var mutexID int64
	if err := tx.GetContext(ctx, &mutexID, "SELECT id FROM auto_sync_configs WHERE id = 1"+lockClause); err != nil {
		return Config{}, fmt.Errorf("lock auto sync config mutex: %w", err)
	}

	var current Config
	if err := tx.GetContext(ctx, &current, autoSyncConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		return Config{}, fmt.Errorf("load current auto sync config: %w", err)
	}
	if current.ID != expectedRevisionID {
		return Config{}, fmt.Errorf("%w: current revision is %d", ErrConfigChanged, current.ID)
	}
	if current.Enabled == enabled {
		if err := tx.Commit(); err != nil {
			return Config{}, fmt.Errorf("commit idempotent auto sync config update: %w", err)
		}
		return current, nil
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO auto_sync_configs (enabled, previous_enabled, created_by, created_at)
		VALUES (?, ?, NULLIF(?, ''), ?)
	`, enabled, current.Enabled, strings.TrimSpace(actor), now)
	if err != nil {
		return Config{}, fmt.Errorf("insert auto sync config revision: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Config{}, fmt.Errorf("read auto sync config revision id: %w", err)
	}
	var updated Config
	if err := tx.GetContext(ctx, &updated, autoSyncConfigSelect+" WHERE id = ?", id); err != nil {
		return Config{}, fmt.Errorf("load auto sync config revision %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Config{}, fmt.Errorf("commit auto sync config update: %w", err)
	}
	if enabled {
		m.wakeWorker()
	}
	return updated, nil
}

const autoSyncConfigSelect = `
	SELECT id, enabled, previous_enabled,
	       COALESCE(created_by, '') AS created_by_value,
	       created_at
	FROM auto_sync_configs`

func (m *Manager) wakeWorker() {
	if m == nil || m.wake == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func autoSyncDeviceTypeArgs(includeZJWA1D bool) []string {
	values := []string{DeviceTypeEgoPortalStereo, DeviceTypeEgoPortalLite}
	if includeZJWA1D {
		values = append(values, DeviceTypeZJWA1D)
	}
	return values
}

func autoSyncDeviceTypeSQL(count int) string {
	return strings.TrimRight(strings.Repeat("HEX(?), ", count), ", ")
}

func supportedDeviceType(deviceType string) bool {
	switch deviceType {
	case DeviceTypeEgoPortalStereo, DeviceTypeEgoPortalLite, DeviceTypeZJWA1D:
		return true
	default:
		return false
	}
}
