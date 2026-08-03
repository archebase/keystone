// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package calibration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

const (
	maxProcessingImageRefLength = 1024

	// ProcessingConfigSourceUnconfigured means no calibration image has been selected.
	ProcessingConfigSourceUnconfigured = "unconfigured"
	// ProcessingConfigSourceDatabase means an administrator-selected revision is effective.
	ProcessingConfigSourceDatabase = "database"
)

var (
	configImageDigestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	configImageRegistryPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)
	configImageRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
)

// ProcessingConfig is one immutable revision of the calibration processing settings.
type ProcessingConfig struct {
	ID                    int64     `db:"id" json:"revision_id"`
	ImageRef              string    `db:"image_ref_value" json:"image_ref,omitempty"`
	PreviousImageRef      string    `db:"previous_image_ref_value" json:"previous_image_ref,omitempty"`
	MaxConcurrent         int       `db:"max_concurrent" json:"max_concurrent"`
	PreviousMaxConcurrent *int      `db:"previous_max_concurrent" json:"previous_max_concurrent,omitempty"`
	CreatedBy             string    `db:"created_by_value" json:"created_by,omitempty"`
	CreatedAt             time.Time `db:"created_at" json:"created_at"`
	Source                string    `db:"-" json:"source"`
}

// CurrentProcessingConfig returns the latest immutable settings revision.
func (m *Manager) CurrentProcessingConfig(ctx context.Context) (ProcessingConfig, error) {
	if m == nil || m.db == nil {
		return ProcessingConfig{}, fmt.Errorf("load calibration processing config: database is not configured")
	}
	var current ProcessingConfig
	if err := m.db.GetContext(ctx, &current, processingConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessingConfig{}, ErrImageNotConfigured
		}
		return ProcessingConfig{}, fmt.Errorf("load current calibration processing config: %w", err)
	}
	return classifyProcessingConfig(current), nil
}

// UpdateProcessingConfig atomically appends an audited settings revision.
func (m *Manager) UpdateProcessingConfig(
	ctx context.Context,
	rawImageRef string,
	maxConcurrent int,
	expectedRevisionID int64,
	actor string,
) (ProcessingConfig, error) {
	if m == nil || m.db == nil {
		return ProcessingConfig{}, fmt.Errorf("update calibration processing config: database is not configured")
	}
	imageRef, err := validateProcessingImageRef(rawImageRef)
	if err != nil {
		return ProcessingConfig{}, err
	}
	if maxConcurrent < 1 || maxConcurrent > MaxConfigurableConcurrent {
		return ProcessingConfig{}, ErrInvalidMaxConcurrent
	}
	m.configMu.Lock()
	defer m.configMu.Unlock()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return ProcessingConfig{}, fmt.Errorf("begin calibration config update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockProcessingConfigMutex(ctx, tx, m.db); err != nil {
		return ProcessingConfig{}, err
	}
	current, err := currentProcessingConfigTx(ctx, tx)
	if err != nil {
		return ProcessingConfig{}, err
	}
	if current.ID != expectedRevisionID {
		return ProcessingConfig{}, fmt.Errorf("%w: current revision is %d", ErrConfigChanged, current.ID)
	}
	if current.ImageRef == imageRef && current.MaxConcurrent == maxConcurrent {
		if err := tx.Commit(); err != nil {
			return ProcessingConfig{}, fmt.Errorf("commit idempotent calibration config update: %w", err)
		}
		return classifyProcessingConfig(current), nil
	}
	created, err := insertProcessingConfigTx(
		ctx,
		tx,
		imageRef,
		current.ImageRef,
		maxConcurrent,
		current.MaxConcurrent,
		strings.TrimSpace(actor),
	)
	if err != nil {
		return ProcessingConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessingConfig{}, fmt.Errorf("commit calibration config update: %w", err)
	}
	m.wakeReconciler()
	return classifyProcessingConfig(created), nil
}

// ListProcessingConfigHistory returns newest-first immutable audit rows.
func (m *Manager) ListProcessingConfigHistory(ctx context.Context, limit, offset int) ([]ProcessingConfig, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("list calibration processing config history: database is not configured")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []ProcessingConfig
	if err := m.db.SelectContext(ctx, &rows, processingConfigSelect+" ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset); err != nil {
		return nil, fmt.Errorf("list calibration processing config history: %w", err)
	}
	for index := range rows {
		rows[index] = classifyProcessingConfig(rows[index])
	}
	return rows, nil
}

const processingConfigSelect = `
	SELECT id,
	       COALESCE(image_ref, '') AS image_ref_value,
	       COALESCE(previous_image_ref, '') AS previous_image_ref_value,
	       max_concurrent,
	       previous_max_concurrent,
	       COALESCE(created_by, '') AS created_by_value,
	       created_at
	FROM calibration_processing_configs`

func lockProcessingConfigMutex(ctx context.Context, tx *sqlx.Tx, db *sqlx.DB) error {
	query := "SELECT id FROM calibration_processing_configs WHERE id = 1" + forUpdateClause(db)
	var id int64
	if err := tx.GetContext(ctx, &id, query); err != nil {
		return fmt.Errorf("lock calibration config mutex: %w", err)
	}
	return nil
}

func currentProcessingConfigTx(ctx context.Context, tx *sqlx.Tx) (ProcessingConfig, error) {
	var config ProcessingConfig
	if err := tx.GetContext(ctx, &config, processingConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		return ProcessingConfig{}, fmt.Errorf("load current calibration config: %w", err)
	}
	return config, nil
}

func insertProcessingConfigTx(
	ctx context.Context,
	tx *sqlx.Tx,
	imageRef string,
	previousImageRef string,
	maxConcurrent int,
	previousMaxConcurrent int,
	actor string,
) (ProcessingConfig, error) {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO calibration_processing_configs (
			image_ref, previous_image_ref, max_concurrent, previous_max_concurrent,
			created_by, created_at
		) VALUES (NULLIF(?, ''), NULLIF(?, ''), ?, ?, NULLIF(?, ''), ?)
	`, imageRef, previousImageRef, maxConcurrent, previousMaxConcurrent, actor, now)
	if err != nil {
		return ProcessingConfig{}, fmt.Errorf("insert calibration config revision: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ProcessingConfig{}, fmt.Errorf("read calibration config revision id: %w", err)
	}
	var created ProcessingConfig
	if err := tx.GetContext(ctx, &created, processingConfigSelect+" WHERE id = ?", id); err != nil {
		return ProcessingConfig{}, fmt.Errorf("load calibration config revision %d: %w", id, err)
	}
	return created, nil
}

func classifyProcessingConfig(config ProcessingConfig) ProcessingConfig {
	if config.ImageRef == "" {
		config.Source = ProcessingConfigSourceUnconfigured
	} else {
		config.Source = ProcessingConfigSourceDatabase
	}
	return config
}

func validateProcessingImageRef(raw string) (string, error) {
	imageRef := strings.TrimSpace(raw)
	if utf8.RuneCountInString(imageRef) > maxProcessingImageRefLength {
		return "", fmt.Errorf("%w: exceeds %d characters", ErrInvalidImageRef, maxProcessingImageRefLength)
	}
	if imageRef == "" || strings.ContainsAny(imageRef, "?#") || strings.Contains(imageRef, "://") ||
		strings.Contains(imageRef, "@") && strings.Count(imageRef, "@") != 1 {
		return "", ErrInvalidImageRef
	}
	parts := strings.Split(imageRef, "@sha256:")
	if len(parts) != 2 || !configImageDigestPattern.MatchString(parts[1]) {
		return "", fmt.Errorf("%w: must use an immutable sha256 digest", ErrInvalidImageRef)
	}
	repository := strings.ToLower(strings.TrimSpace(parts[0]))
	segments := strings.Split(repository, "/")
	if len(segments) < 2 || !configImageRegistryPattern.MatchString(segments[0]) ||
		!configImageRepositoryPattern.MatchString(repository) {
		return "", fmt.Errorf("%w: invalid repository", ErrInvalidImageRef)
	}
	if strings.Contains(repository, "..") || strings.Contains(repository, "@") || strings.Contains(repository, "\\") {
		return "", fmt.Errorf("%w: invalid repository", ErrInvalidImageRef)
	}
	return repository + "@sha256:" + parts[1], nil
}
