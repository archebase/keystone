// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package handlers

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseNonEmptyStringListBoundsValuesBeforeDedup(t *testing.T) {
	raw := joinedStringList("device", maxMultiValueFilterItems)
	values, err := parseNonEmptyStringList(raw, "device_id")
	if err != nil {
		t.Fatalf("parseNonEmptyStringList returned error: %v", err)
	}
	if len(values) != maxMultiValueFilterItems {
		t.Fatalf("value count = %d, want %d", len(values), maxMultiValueFilterItems)
	}

	if _, err := parseNonEmptyStringList(joinedStringList("device", maxMultiValueFilterItems+1), "device_id"); err == nil {
		t.Fatalf("expected too many values error")
	}
	if _, err := parseNonEmptyStringList(strings.Repeat("same,", maxMultiValueFilterItems+1), "device_id"); err == nil {
		t.Fatalf("expected repeated values to count toward the limit")
	}
	if _, err := parseNonEmptyStringList(strings.Repeat("x", maxMultiValueFilterStringItemLength+1), "device_id"); err == nil {
		t.Fatalf("expected oversized string item error")
	}
}

func joinedStringList(prefix string, count int) string {
	values := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		values = append(values, prefix+strconv.Itoa(i))
	}
	return strings.Join(values, ",")
}
