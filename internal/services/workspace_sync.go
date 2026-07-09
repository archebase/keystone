// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"archebase.com/keystone-edge/internal/auth"
	"archebase.com/keystone-edge/internal/config"
	"archebase.com/keystone-edge/internal/logger"
	"github.com/jmoiron/sqlx"
)

const (
	defaultWorkspaceID          int64  = 0
	defaultWorkspaceName        string = "Default Workspace"
	defaultWorkspaceDescription string = "Local-only fallback workspace"

	workspaceSourceDefault string = "default"
	workspaceSourceHilbert string = "hilbert"
)

var (
	// ErrWorkspaceSyncNotConfigured indicates Hilbert service identity bootstrap is incomplete.
	ErrWorkspaceSyncNotConfigured = errors.New("workspace sync not configured")
	// ErrWorkspaceSyncFailed indicates Hilbert workspace sync failed.
	ErrWorkspaceSyncFailed = errors.New("workspace sync failed")
)

// HilbertWorkspaceClient captures the Hilbert calls workspace sync needs.
type HilbertWorkspaceClient interface {
	Configured() bool
	Login(ctx context.Context, code string, password string) (*auth.HilbertLoginResult, error)
	ListAvailableWorkspaces(ctx context.Context, sessionKey string) ([]auth.HilbertWorkspace, error)
}

// WorkspaceSyncResult summarizes one Hilbert workspace sync run.
type WorkspaceSyncResult struct {
	SyncedCount     int
	DefaultIncluded bool
	LastSyncedAt    time.Time
	ResourceSync    *WorkspaceResourceSyncSummary
}

// WorkspaceSyncService syncs Hilbert workspace projections into Keystone.
type WorkspaceSyncService struct {
	db            *sqlx.DB
	cfg           *config.HilbertConfig
	hilbertClient HilbertWorkspaceClient
	resourceSync  *WorkspaceResourceSyncService
}

// NewWorkspaceSyncService creates a WorkspaceSyncService.
func NewWorkspaceSyncService(db *sqlx.DB, cfg *config.HilbertConfig, hilbertClient HilbertWorkspaceClient) *WorkspaceSyncService {
	if hilbertClient == nil {
		hilbertClient = auth.NewHilbertClient(cfg)
	}
	var resourceSync *WorkspaceResourceSyncService
	if resourceClient, ok := hilbertClient.(HilbertWorkspaceResourceClient); ok {
		resourceSync = NewWorkspaceResourceSyncService(db, resourceClient)
	}
	return &WorkspaceSyncService{db: db, cfg: cfg, hilbertClient: hilbertClient, resourceSync: resourceSync}
}

// Configured reports whether startup/manual sync has the required Hilbert service identity settings.
func (s *WorkspaceSyncService) Configured() bool {
	if s == nil || s.db == nil || s.cfg == nil || s.hilbertClient == nil || !s.hilbertClient.Configured() {
		return false
	}
	return strings.TrimSpace(s.cfg.BaseURL) != "" &&
		strings.TrimSpace(s.cfg.ServiceAccountCode) != "" &&
		strings.TrimSpace(s.cfg.ServiceAccountPassword) != ""
}

// Sync logs into Hilbert, fetches available workspaces, validates every record, and transactionally upserts them.
func (s *WorkspaceSyncService) Sync(ctx context.Context) (*WorkspaceSyncResult, error) {
	if !s.Configured() {
		return nil, ErrWorkspaceSyncNotConfigured
	}

	now := time.Now().UTC()
	if err := ensureDefaultWorkspace(ctx, s.db, now); err != nil {
		return nil, fmt.Errorf("%w: ensure default workspace: %v", ErrWorkspaceSyncFailed, err)
	}

	loginResult, err := s.hilbertClient.Login(ctx, s.cfg.ServiceAccountCode, s.cfg.ServiceAccountPassword)
	if err != nil {
		return nil, fmt.Errorf("%w: login hilbert: %v", ErrWorkspaceSyncFailed, err)
	}
	sessionKey := loginResult.SessionKey()
	if strings.TrimSpace(sessionKey) == "" {
		return nil, fmt.Errorf("%w: login hilbert: missing session key", ErrWorkspaceSyncFailed)
	}
	logger.Printf("[WORKSPACE] Hilbert service identity login succeeded: code=%s", s.cfg.ServiceAccountCode)
	workspaces, err := s.hilbertClient.ListAvailableWorkspaces(ctx, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("%w: list hilbert workspaces: %v", ErrWorkspaceSyncFailed, err)
	}
	logger.Printf("[WORKSPACE] Hilbert workspace list fetched: count=%d", len(workspaces))

	if err := validateHilbertWorkspaces(workspaces); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkspaceSyncFailed, err)
	}
	if err := s.upsertHilbertWorkspaces(ctx, workspaces, now); err != nil {
		return nil, fmt.Errorf("%w: upsert hilbert workspaces: %v", ErrWorkspaceSyncFailed, err)
	}
	logger.Printf("[WORKSPACE] Hilbert workspace sync upsert committed: synced_count=%d", len(workspaces))

	resourceSummary := &WorkspaceResourceSyncSummary{Enabled: true, WorkspaceResults: []WorkspaceResourceSyncResult{}}
	if s.resourceSync != nil {
		resourceSummary = s.resourceSync.SyncWorkspaces(ctx, sessionKey, workspaces, now)
	}

	return &WorkspaceSyncResult{
		SyncedCount:     len(workspaces),
		DefaultIncluded: true,
		LastSyncedAt:    now,
		ResourceSync:    resourceSummary,
	}, nil
}

func ensureDefaultWorkspace(ctx context.Context, db *sqlx.DB, now time.Time) error {
	result, err := db.ExecContext(ctx, `
		UPDATE workspaces
		SET
			name = ?,
			description = ?,
			source = ?,
			admins_str = ?,
			members_str = ?,
			deleted_at = NULL,
			updated_at = ?
		WHERE id = ?
	`,
		defaultWorkspaceName,
		defaultWorkspaceDescription,
		workspaceSourceDefault,
		sql.NullString{},
		sql.NullString{},
		now,
		defaultWorkspaceID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO workspaces (
			id,
			name,
			description,
			source,
			admins_str,
			members_str,
			last_synced_at,
			created_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		defaultWorkspaceID,
		defaultWorkspaceName,
		defaultWorkspaceDescription,
		workspaceSourceDefault,
		sql.NullString{},
		sql.NullString{},
		sql.NullTime{},
		now,
		now,
	)
	return err
}

func validateHilbertWorkspaces(workspaces []auth.HilbertWorkspace) error {
	seen := make(map[int64]struct{}, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.ID <= defaultWorkspaceID {
			return fmt.Errorf("invalid hilbert workspace id %d", workspace.ID)
		}
		if strings.TrimSpace(workspace.Name) == "" {
			return fmt.Errorf("hilbert workspace %d has empty name", workspace.ID)
		}
		if _, ok := seen[workspace.ID]; ok {
			return fmt.Errorf("duplicate hilbert workspace id %d", workspace.ID)
		}
		seen[workspace.ID] = struct{}{}
	}
	return nil
}

func (s *WorkspaceSyncService) upsertHilbertWorkspaces(ctx context.Context, workspaces []auth.HilbertWorkspace, syncedAt time.Time) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, workspace := range workspaces {
		if err := upsertHilbertWorkspace(ctx, tx, workspace, syncedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertHilbertWorkspace(ctx context.Context, tx *sqlx.Tx, workspace auth.HilbertWorkspace, syncedAt time.Time) error {
	description := sql.NullString{}
	if workspace.Description != nil && strings.TrimSpace(*workspace.Description) != "" {
		description = sql.NullString{String: strings.TrimSpace(*workspace.Description), Valid: true}
	}

	hilbertCreatedAt := sql.NullTime{}
	if !workspace.CreatedTime.IsZero() {
		hilbertCreatedAt = sql.NullTime{Time: workspace.CreatedTime.UTC(), Valid: true}
	}
	hilbertUpdatedAt := sql.NullTime{}
	if workspace.UpdatedTime != nil && !workspace.UpdatedTime.IsZero() {
		hilbertUpdatedAt = sql.NullTime{Time: workspace.UpdatedTime.UTC(), Valid: true}
	}

	args := []any{
		workspace.ID,
		strings.TrimSpace(workspace.Name),
		description,
		workspaceSourceHilbert,
		nullableHashWrappedString(workspace.Admins),
		nullableHashWrappedString(workspace.Members),
		syncedAt,
		hilbertCreatedAt,
		hilbertUpdatedAt,
		syncedAt,
		syncedAt,
	}

	if tx.DriverName() == "sqlite" {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO workspaces (
				id, name, description, source, admins_str, members_str,
				last_synced_at, hilbert_created_at, hilbert_updated_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				description = excluded.description,
				source = excluded.source,
				admins_str = excluded.admins_str,
				members_str = excluded.members_str,
				last_synced_at = excluded.last_synced_at,
				hilbert_created_at = excluded.hilbert_created_at,
				hilbert_updated_at = excluded.hilbert_updated_at,
				updated_at = excluded.updated_at,
				deleted_at = NULL
		`, args...)
		return err
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (
			id, name, description, source, admins_str, members_str,
			last_synced_at, hilbert_created_at, hilbert_updated_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			name = VALUES(name),
			description = VALUES(description),
			source = VALUES(source),
			admins_str = VALUES(admins_str),
			members_str = VALUES(members_str),
			last_synced_at = VALUES(last_synced_at),
			hilbert_created_at = VALUES(hilbert_created_at),
			hilbert_updated_at = VALUES(hilbert_updated_at),
			updated_at = VALUES(updated_at),
			deleted_at = NULL
	`, args...)
	return err
}

func nullableHashWrappedString(values []string) sql.NullString {
	normalized := normalizeWorkspacePeople(values)
	if len(normalized) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: "#" + strings.Join(normalized, "#") + "#", Valid: true}
}

func normalizeWorkspacePeople(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		item := strings.TrimSpace(value)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		normalized = append(normalized, item)
	}
	return normalized
}
