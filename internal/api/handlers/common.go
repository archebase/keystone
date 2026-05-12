// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

const (
	defaultLimit                         = 50
	maxLimit                             = 100
	maxSlugLength                        = 100
	maxMultiValueFilterItems             = 100
	maxMultiValueFilterRawLength         = 8192
	maxMultiValueFilterStringItemLength  = 255
	maxMultiValueFilterIntegerItemLength = 20
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

func firstNonEmptyQuery(c *gin.Context, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value
		}
	}
	return ""
}

func appendKeywordSearch(whereClause string, args []any, keyword string, fields ...string) (string, []any) {
	if keyword == "" || len(fields) == 0 {
		return whereClause, args
	}

	conditions := make([]string, 0, len(fields))
	likeKeyword := "%" + keyword + "%"
	for _, field := range fields {
		conditions = append(conditions, field+" LIKE ?")
		args = append(args, likeKeyword)
	}
	return whereClause + " AND (" + strings.Join(conditions, " OR ") + ")", args
}

func parsePositiveInt64List(raw string, fieldName string) ([]int64, error) {
	items, err := splitBoundedMultiValueItems([]string{raw}, fieldName, maxMultiValueFilterIntegerItemLength)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	seen := make(map[int64]struct{})
	values := []int64{}
	for _, item := range items {
		parsed, err := strconv.ParseInt(item, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid %s format", fieldName)
		}
		if _, ok := seen[parsed]; ok {
			continue
		}
		seen[parsed] = struct{}{}
		values = append(values, parsed)
	}
	return values, nil
}

func parseNonEmptyStringList(raw string, fieldName string) ([]string, error) {
	return parseNonEmptyStringListValues([]string{raw}, fieldName)
}

func parseNonEmptyStringListValues(rawValues []string, fieldName string) ([]string, error) {
	items, err := splitBoundedMultiValueItems(rawValues, fieldName, maxMultiValueFilterStringItemLength)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{})
	values := []string{}
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	return values, nil
}

func splitBoundedMultiValueItems(rawValues []string, fieldName string, maxItemLength int) ([]string, error) {
	totalRawLength := 0
	items := []string{}
	for _, raw := range rawValues {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		totalRawLength += len(raw)
		if totalRawLength > maxMultiValueFilterRawLength {
			return nil, fmt.Errorf("%s query is too long", fieldName)
		}
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if len(item) > maxItemLength {
				return nil, fmt.Errorf("%s contains a value longer than %d characters", fieldName, maxItemLength)
			}
			items = append(items, item)
			if len(items) > maxMultiValueFilterItems {
				return nil, fmt.Errorf("%s contains too many values; maximum is %d", fieldName, maxMultiValueFilterItems)
			}
		}
	}
	return items, nil
}

func appendInt64InFilter(whereClause string, args []any, column string, values []int64) (string, []any) {
	if len(values) == 0 {
		return whereClause, args
	}

	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return whereClause + " AND " + column + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func appendStringInFilter(whereClause string, args []any, column string, values []string) (string, []any) {
	if len(values) == 0 {
		return whereClause, args
	}

	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		args = append(args, value)
	}
	return whereClause + " AND " + column + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func keywordOrderBy(keyword string, fallback string, fields ...string) (string, []any) {
	if keyword == "" || len(fields) == 0 {
		return "ORDER BY " + fallback, nil
	}

	clauses := make([]string, 0, len(fields)*2)
	args := make([]any, 0, len(fields)*2)
	prefixKeyword := keyword + "%"
	rank := 0
	for _, field := range fields {
		clauses = append(clauses, fmt.Sprintf("WHEN %s = ? THEN %d", field, rank))
		args = append(args, keyword)
		rank++
		clauses = append(clauses, fmt.Sprintf("WHEN %s LIKE ? THEN %d", field, rank))
		args = append(args, prefixKeyword)
		rank++
	}

	return fmt.Sprintf("ORDER BY CASE %s ELSE %d END, %s", strings.Join(clauses, " "), rank, fallback), args
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
