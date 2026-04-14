// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"testing"
	"time"
)

func TestStripBucketPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"edge-factory-default/factory-default/device/2024-01-01/task.mcap", "factory-default/device/2024-01-01/task.mcap"},
		{"/edge-factory-default/factory-default/device/2024-01-01/task.mcap", "factory-default/device/2024-01-01/task.mcap"},
		{"bucket/key", "key"},
		{"just-a-file.mcap", "just-a-file.mcap"},
		{"  ", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := stripBucketPrefix(tt.input)
		if got != tt.want {
			t.Errorf("stripBucketPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEnqueueEpisode_DeduplicatesPendingEpisode(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan int64, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 42); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if err := w.EnqueueEpisode(ctx, 42); err != nil {
		t.Fatalf("second enqueue failed: %v", err)
	}

	select {
	case got := <-w.enqueueCh:
		if got != 42 {
			t.Fatalf("unexpected episode id: got %d want 42", got)
		}
	default:
		t.Fatal("expected episode to be enqueued")
	}

	select {
	case got := <-w.enqueueCh:
		t.Fatalf("duplicate enqueue detected: got %d", got)
	default:
	}
}

func TestEnqueueEpisode_AllowsReenqueueAfterProcessing(t *testing.T) {
	w := &SyncWorker{
		enqueueCh:       make(chan int64, 2),
		enqueuedEpisode: make(map[int64]struct{}),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("initial enqueue failed: %v", err)
	}
	w.unmarkEnqueued(7)
	if err := w.EnqueueEpisode(ctx, 7); err != nil {
		t.Fatalf("reenqueue failed: %v", err)
	}

	count := 0
	for {
		select {
		case <-w.enqueueCh:
			count++
		default:
			if count != 2 {
				t.Fatalf("expected 2 enqueue records after reenqueue, got %d", count)
			}
			return
		}
	}
}
