// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUploadRecordingTimeRange(t *testing.T) {
	startedAt, finishedAt, err := uploadRecordingTimeRange(map[string]string{
		"recording_started_at":  "2026-08-25T10:00:00.123Z",
		"recording_finished_at": "2026-08-25T18:00:06.523+08:00",
	})
	if err != nil {
		t.Fatalf("uploadRecordingTimeRange() error = %v", err)
	}
	if !startedAt.Valid || !finishedAt.Valid || startedAt.Time.Location() != time.UTC || finishedAt.Time.Location() != time.UTC {
		t.Fatalf("normalized times = %#v %#v, want valid UTC values", startedAt, finishedAt)
	}
	if got := finishedAt.Time.Sub(startedAt.Time); got != 6*time.Second+400*time.Millisecond {
		t.Fatalf("time range duration = %s, want 6.4s", got)
	}

	for name, tags := range map[string]map[string]string{
		"missing finish": {"recording_started_at": "2026-08-25T10:00:00Z"},
		"invalid start": {
			"recording_started_at":  "not-a-time",
			"recording_finished_at": "2026-08-25T10:00:01Z",
		},
		"reversed": {
			"recording_started_at":  "2026-08-25T10:00:01Z",
			"recording_finished_at": "2026-08-25T10:00:00Z",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := uploadRecordingTimeRange(tags)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("uploadRecordingTimeRange() error = %v, want InvalidArgument", err)
			}
		})
	}
}
