// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

type workspaceAccessQueryer interface {
	sqlx.QueryerContext
}

type workspaceAccessRow struct {
	ID      int64  `db:"id"`
	Admins  string `db:"admins"`
	Members string `db:"members"`
}

// EncodeWorkspacePeople normalizes account codes and serializes them as a JSON array.
func EncodeWorkspacePeople(values []string) (string, error) {
	normalized := normalizeWorkspacePeople(values)
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("encode workspace people: %w", err)
	}
	return string(data), nil
}

// DecodeWorkspacePeople parses one Workspace admins or members JSON array.
func DecodeWorkspacePeople(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("decode workspace people: %w", err)
	}
	return normalizeWorkspacePeople(values), nil
}

// OperatorHasWorkspaceAccess reports whether an operator is an admin or member of a Workspace.
func OperatorHasWorkspaceAccess(ctx context.Context, q workspaceAccessQueryer, operatorID string, workspaceID int64) (bool, error) {
	operatorID = strings.TrimSpace(operatorID)
	if operatorID == "" {
		return false, nil
	}
	var row workspaceAccessRow
	if err := sqlx.GetContext(ctx, q, &row, `
		SELECT id, admins, members
		FROM workspaces
		WHERE id = ? AND deleted_at IS NULL
	`, workspaceID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("query workspace access: %w", err)
	}
	return workspaceRowContainsOperator(row, operatorID)
}

// AccessibleWorkspaceIDs returns every active Workspace an operator can access.
func AccessibleWorkspaceIDs(ctx context.Context, q workspaceAccessQueryer, operatorID string) ([]int64, error) {
	operatorID = strings.TrimSpace(operatorID)
	if operatorID == "" {
		return []int64{}, nil
	}
	rows := []workspaceAccessRow{}
	if err := sqlx.SelectContext(ctx, q, &rows, `
		SELECT id, admins, members
		FROM workspaces
		WHERE deleted_at IS NULL
		ORDER BY id
	`); err != nil {
		return nil, fmt.Errorf("list workspace access: %w", err)
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		allowed, err := workspaceRowContainsOperator(row, operatorID)
		if err != nil {
			return nil, fmt.Errorf("workspace %d access: %w", row.ID, err)
		}
		if allowed {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// WorkspaceOperatorIDs returns the deduplicated admins and members of selected Workspaces.
func WorkspaceOperatorIDs(ctx context.Context, q workspaceAccessQueryer, workspaceIDs []int64) ([]string, error) {
	if len(workspaceIDs) == 0 {
		return []string{}, nil
	}
	query, args, err := sqlx.In(`
		SELECT id, admins, members
		FROM workspaces
		WHERE id IN (?) AND deleted_at IS NULL
	`, workspaceIDs)
	if err != nil {
		return nil, fmt.Errorf("build workspace people query: %w", err)
	}
	rows := []workspaceAccessRow{}
	if err := sqlx.SelectContext(ctx, q, &rows, qRebind(q, query), args...); err != nil {
		return nil, fmt.Errorf("query workspace people: %w", err)
	}
	seen := map[string]struct{}{}
	for _, row := range rows {
		for _, raw := range []string{row.Admins, row.Members} {
			values, err := DecodeWorkspacePeople(raw)
			if err != nil {
				return nil, fmt.Errorf("workspace %d people: %w", row.ID, err)
			}
			for _, value := range values {
				seen[value] = struct{}{}
			}
		}
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

func workspaceRowContainsOperator(row workspaceAccessRow, operatorID string) (bool, error) {
	for _, raw := range []string{row.Admins, row.Members} {
		values, err := DecodeWorkspacePeople(raw)
		if err != nil {
			return false, err
		}
		for _, value := range values {
			if value == operatorID {
				return true, nil
			}
		}
	}
	return false, nil
}

func qRebind(q workspaceAccessQueryer, query string) string {
	if rebinder, ok := q.(interface{ Rebind(string) string }); ok {
		return rebinder.Rebind(query)
	}
	return query
}
