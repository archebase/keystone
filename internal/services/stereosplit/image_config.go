// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// ImageConfigSourceUnconfigured means no processing image has been selected.
	ImageConfigSourceUnconfigured = "unconfigured"
	// ImageConfigSourceDatabase means an administrator-selected database revision is effective.
	ImageConfigSourceDatabase = "database"
)

var (
	imageDigestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imageRegistryPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)
	imageRepositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)
)

// ImageConfig is one immutable revision of the stereo-split processing settings.
type ImageConfig struct {
	ID                            int64     `db:"id" json:"revision_id"`
	ImageRef                      string    `db:"image_ref_value" json:"image_ref,omitempty"`
	PreviousImageRef              string    `db:"previous_image_ref_value" json:"previous_image_ref,omitempty"`
	MaxConcurrent                 int       `db:"max_concurrent" json:"max_concurrent"`
	PreviousMaxConcurrent         *int      `db:"previous_max_concurrent" json:"previous_max_concurrent,omitempty"`
	ResourceLimitsEnabled         bool      `db:"resource_limits_enabled" json:"resource_limits_enabled"`
	PreviousResourceLimitsEnabled *bool     `db:"previous_resource_limits_enabled" json:"previous_resource_limits_enabled,omitempty"`
	CreatedBy                     string    `db:"created_by_value" json:"created_by,omitempty"`
	CreatedAt                     time.Time `db:"created_at" json:"created_at"`
	Source                        string    `db:"-" json:"source"`
}

// CurrentImageConfig returns the immutable current processing-settings revision.
func (m *Manager) CurrentImageConfig(ctx context.Context) (ImageConfig, error) {
	if m == nil || m.db == nil {
		return ImageConfig{}, fmt.Errorf("load stereo split image: database is not configured")
	}
	var current ImageConfig
	if err := m.db.GetContext(ctx, &current, imageConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImageConfig{}, ErrImageNotConfigured
		}
		return ImageConfig{}, fmt.Errorf("load current stereo split image: %w", err)
	}
	return classifyImageConfig(current), nil
}

// UpdateImageConfig atomically appends a new audited processing-settings
// revision. The migration bootstrap row is locked as a singleton mutex so
// correctness does not depend on InnoDB gap-lock behavior for MAX(id).
func (m *Manager) UpdateImageConfig(ctx context.Context, rawImageRef string, maxConcurrent int, resourceLimitsEnabled bool, expectedRevisionID int64, actor string) (ImageConfig, error) {
	if m == nil || m.db == nil {
		return ImageConfig{}, fmt.Errorf("update stereo split image: database is not configured")
	}
	imageRef, err := validateImageRef(rawImageRef)
	if err != nil {
		return ImageConfig{}, err
	}
	if err := validateMaxConcurrent(maxConcurrent); err != nil {
		return ImageConfig{}, err
	}
	m.dispatchMu.Lock()
	defer m.dispatchMu.Unlock()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return ImageConfig{}, fmt.Errorf("begin image config update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockImageConfigMutex(ctx, tx, m.db); err != nil {
		return ImageConfig{}, err
	}
	current, err := currentImageConfigTx(ctx, tx)
	if err != nil {
		return ImageConfig{}, err
	}
	if current.ID != expectedRevisionID {
		return ImageConfig{}, fmt.Errorf("%w: current revision is %d", ErrConfigChanged, current.ID)
	}
	if current.ImageRef == imageRef && current.MaxConcurrent == maxConcurrent && current.ResourceLimitsEnabled == resourceLimitsEnabled {
		if err := tx.Commit(); err != nil {
			return ImageConfig{}, fmt.Errorf("commit idempotent image update: %w", err)
		}
		return classifyImageConfig(current), nil
	}
	created, err := insertImageConfigTx(
		ctx,
		tx,
		imageRef,
		current.ImageRef,
		maxConcurrent,
		current.MaxConcurrent,
		resourceLimitsEnabled,
		current.ResourceLimitsEnabled,
		strings.TrimSpace(actor),
	)
	if err != nil {
		return ImageConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImageConfig{}, fmt.Errorf("commit image config update: %w", err)
	}
	m.wakeReconciler()
	return classifyImageConfig(created), nil
}

// ListImageConfigHistory returns newest-first immutable settings audit rows.
func (m *Manager) ListImageConfigHistory(ctx context.Context, limit, offset int) ([]ImageConfig, error) {
	if m == nil || m.db == nil {
		return nil, fmt.Errorf("list stereo split image history: database is not configured")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []ImageConfig
	if err := m.db.SelectContext(ctx, &rows, imageConfigSelect+" ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset); err != nil {
		return nil, fmt.Errorf("list stereo split image history: %w", err)
	}
	for i := range rows {
		rows[i] = classifyImageConfig(rows[i])
	}
	return rows, nil
}

const imageConfigSelect = `
	SELECT id,
	       COALESCE(image_ref, '') AS image_ref_value,
	       COALESCE(previous_image_ref, '') AS previous_image_ref_value,
	       max_concurrent,
	       previous_max_concurrent,
	       resource_limits_enabled,
	       previous_resource_limits_enabled,
	       COALESCE(created_by, '') AS created_by_value,
	       created_at
	FROM stereo_split_image_configs`

func lockImageConfigMutex(ctx context.Context, tx *sqlx.Tx, db *sqlx.DB) error {
	query := "SELECT id FROM stereo_split_image_configs WHERE id = 1" + forUpdateClause(db)
	var id int64
	if err := tx.GetContext(ctx, &id, query); err != nil {
		return fmt.Errorf("lock image config mutex: %w", err)
	}
	return nil
}

func currentImageConfigTx(ctx context.Context, tx *sqlx.Tx) (ImageConfig, error) {
	var config ImageConfig
	if err := tx.GetContext(ctx, &config, imageConfigSelect+" ORDER BY id DESC LIMIT 1"); err != nil {
		return ImageConfig{}, fmt.Errorf("load current image config: %w", err)
	}
	return config, nil
}

func insertImageConfigTx(
	ctx context.Context,
	tx *sqlx.Tx,
	imageRef string,
	previousImageRef string,
	maxConcurrent int,
	previousMaxConcurrent int,
	resourceLimitsEnabled bool,
	previousResourceLimitsEnabled bool,
	actor string,
) (ImageConfig, error) {
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO stereo_split_image_configs (
			image_ref, previous_image_ref, max_concurrent, previous_max_concurrent,
			resource_limits_enabled, previous_resource_limits_enabled, created_by, created_at
		) VALUES (NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?)
	`, imageRef, previousImageRef, maxConcurrent, previousMaxConcurrent,
		resourceLimitsEnabled, previousResourceLimitsEnabled, actor, now)
	if err != nil {
		return ImageConfig{}, fmt.Errorf("insert image config revision: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read image config revision id: %w", err)
	}
	var created ImageConfig
	if err := tx.GetContext(ctx, &created, imageConfigSelect+" WHERE id = ?", id); err != nil {
		return ImageConfig{}, fmt.Errorf("load image config revision %d: %w", id, err)
	}
	return created, nil
}

func configuredMaxConcurrent(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}

func validateMaxConcurrent(value int) error {
	if value < 1 || value > MaxConfigurableConcurrent {
		return ErrInvalidMaxConcurrent
	}
	return nil
}

func classifyImageConfig(config ImageConfig) ImageConfig {
	if config.ImageRef == "" {
		config.Source = ImageConfigSourceUnconfigured
	} else {
		config.Source = ImageConfigSourceDatabase
	}
	return config
}

func validateImageRef(raw string) (string, error) {
	imageRef := strings.TrimSpace(raw)
	if imageRef == "" || strings.ContainsAny(imageRef, "?#") || strings.Contains(imageRef, "://") || strings.Contains(imageRef, "@") && strings.Count(imageRef, "@") != 1 {
		return "", fmt.Errorf("invalid image reference")
	}
	parts := strings.Split(imageRef, "@sha256:")
	if len(parts) != 2 || !imageDigestPattern.MatchString(parts[1]) {
		return "", fmt.Errorf("image reference must use an immutable sha256 digest")
	}
	repository := strings.ToLower(strings.TrimSpace(parts[0]))
	segments := strings.Split(repository, "/")
	if len(segments) < 2 || !imageRegistryPattern.MatchString(segments[0]) || !imageRepositoryPattern.MatchString(repository) {
		return "", fmt.Errorf("invalid image repository")
	}
	if strings.Contains(repository, "..") || strings.Contains(repository, "@") || strings.Contains(repository, "\\") {
		return "", fmt.Errorf("invalid image repository")
	}
	return repository + "@sha256:" + parts[1], nil
}
