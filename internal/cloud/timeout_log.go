// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package cloud

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.DeadlineExceeded {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}

func timeoutDuration(ctx context.Context, start time.Time, configured time.Duration) time.Duration {
	timeout := configured
	if deadline, ok := ctx.Deadline(); ok {
		if d := deadline.Sub(start); d > 0 && (timeout <= 0 || d < timeout) {
			timeout = d.Round(time.Millisecond)
		}
	}
	if timeout > 0 {
		return timeout
	}
	if !start.IsZero() {
		return time.Since(start).Round(time.Millisecond)
	}
	return 0
}

func timeoutLogValue(timeout time.Duration) string {
	if timeout <= 0 {
		return "unknown"
	}
	return timeout.String()
}

func timeoutLogMilliseconds(timeout time.Duration) int64 {
	if timeout <= 0 {
		return 0
	}
	return timeout.Milliseconds()
}
