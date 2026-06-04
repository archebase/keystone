// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"errors"
	"fmt"
)

type syncNonRetryableError struct {
	err error
}

func (e *syncNonRetryableError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *syncNonRetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newNonRetryableSyncError(format string, args ...interface{}) error {
	return &syncNonRetryableError{err: fmt.Errorf(format, args...)}
}

func wrapNonRetryableSyncError(err error, format string, args ...interface{}) error {
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	return &syncNonRetryableError{err: fmt.Errorf("%s: %w", msg, err)}
}

func isNonRetryableSyncError(err error) bool {
	var target *syncNonRetryableError
	return errors.As(err, &target)
}
