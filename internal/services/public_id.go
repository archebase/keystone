// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewPublicTaskID returns a human-readable task ID with enough entropy for concurrent creation.
func NewPublicTaskID(now time.Time, seq int) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"task_%s_%03d_%02d_%s",
		now.UTC().Format("20060102_150405"),
		now.UTC().Nanosecond()/1_000_000,
		seq%100,
		hex.EncodeToString(b),
	), nil
}

// NewPublicBatchID returns a human-readable batch ID with enough entropy for concurrent creation.
func NewPublicBatchID(now time.Time, seq int) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"batch_%s_%03d_%02d_%s",
		now.UTC().Format("20060102_150405"),
		now.UTC().Nanosecond()/1_000_000,
		seq%100,
		hex.EncodeToString(b),
	), nil
}
