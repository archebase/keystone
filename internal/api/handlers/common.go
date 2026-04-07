// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxSlugLength matches VARCHAR(100) for slug columns in the schema.
const maxSlugLength = 100

// invalidSlugUserMessage is returned when slug fails isValidSlug (length or charset).
const invalidSlugUserMessage = "slug must be at most 100 characters and contain only letters, digits, and hyphens"

// isValidSlug checks non-empty slug, length <= maxSlugLength (in runes), and allows Unicode letters/digits plus hyphen.
func isValidSlug(s string) bool {
	if s == "" {
		return false
	}
	if utf8.RuneCountInString(s) > maxSlugLength {
		return false
	}
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '-' {
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
