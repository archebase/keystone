// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	defaultLimit  = 50
	maxLimit      = 100
	maxSlugLength = 100
)

const invalidSlugUserMessage = "slug must be at most 100 characters and contain only letters, digits, and hyphens"

// PaginationParams represents normalized pagination input from query parameters.
type PaginationParams struct {
	Limit  int
	Offset int
}

// ListResponse is a generic list payload with pagination metadata.
type ListResponse struct {
	Items   interface{} `json:"items"`
	Total   int         `json:"total"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
	HasNext bool        `json:"hasNext,omitempty"`
	HasPrev bool        `json:"hasPrev,omitempty"`
}

// ParsePagination reads and validates pagination query parameters from the request.
func ParsePagination(c *gin.Context) (PaginationParams, error) {
	limit := defaultLimit
	offset := 0

	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit < 1 {
			return PaginationParams{}, &PaginationError{Message: "limit must be a positive integer"}
		}
		if parsedLimit > maxLimit {
			parsedLimit = maxLimit
		}
		limit = parsedLimit
	}

	if rawOffset := strings.TrimSpace(c.Query("offset")); rawOffset != "" {
		parsedOffset, err := strconv.Atoi(rawOffset)
		if err != nil || parsedOffset < 0 {
			return PaginationParams{}, &PaginationError{Message: "offset must be a non-negative integer"}
		}
		offset = parsedOffset
	}

	return PaginationParams{Limit: limit, Offset: offset}, nil
}

// PaginationError is returned when pagination inputs are invalid.
type PaginationError struct {
	Message string
}

func (e *PaginationError) Error() string {
	return e.Message
}

// PaginationErrorResponse writes a standardized 400 response for pagination errors.
func PaginationErrorResponse(c *gin.Context, err error) {
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

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
