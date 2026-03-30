// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// isValidSlug checks if the slug contains only alphanumeric characters and hyphens.
func isValidSlug(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// parseJSONRaw parses a JSON string and returns it as a raw interface{}.
func parseJSONRaw(s string) interface{} {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var result interface{}
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return s
	}
	return result
}

func parseJSONArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		return nil
	}
	return result
}

// jsonStringOrEmptyObject returns JSON text for sensor_suite/capabilities.
// Empty or null raw defaults to {}.
func jsonStringOrEmptyObject(raw json.RawMessage) string {
	ns := sqlNullJSONFromRaw(raw)
	if !ns.Valid {
		return "{}"
	}
	return ns.String
}

func sqlNullJSONFromRaw(raw json.RawMessage) sql.NullString {
	if len(raw) == 0 {
		return sql.NullString{Valid: false}
	}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: s, Valid: true}
}

func formatDBTimeToRFC3339(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// MySQL commonly returns "YYYY-MM-DD HH:MM:SS" or with fractional seconds.
	// Some drivers/configs may return RFC3339.
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999999999",
		time.RFC3339Nano,
		time.RFC3339,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}

	// Fallback: return original string instead of a wrong timestamp.
	return s
}
