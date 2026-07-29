// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestSourceObjectRangeReaderUsesFreshContextForEveryRange(t *testing.T) {
	source := &fakeSourceObjectReader{data: []byte("abcdefghij")}
	reader, err := newSourceObjectRangeReader(
		context.Background(),
		source,
		"source-bucket",
		"capture.mcap",
		int64(len(source.data)),
		source.objectETag(),
		4,
		time.Second,
	)
	if err != nil {
		t.Fatalf("newSourceObjectRangeReader() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	first := make([]byte, 4)
	if _, err := io.ReadFull(reader, first); err != nil {
		t.Fatalf("read first range: %v", err)
	}
	if string(first) != "abcd" {
		t.Fatalf("first range = %q, want abcd", first)
	}
	if len(source.ranges) != 1 || !errors.Is(source.ranges[0].ctx.Err(), context.Canceled) {
		t.Fatalf("first range context error = %v, want canceled after range close", source.ranges[0].ctx.Err())
	}
	firstDeadline, ok := source.ranges[0].ctx.Deadline()
	if !ok {
		t.Fatal("first range context has no deadline")
	}

	time.Sleep(10 * time.Millisecond)
	second := make([]byte, 4)
	if _, err := io.ReadFull(reader, second); err != nil {
		t.Fatalf("read second range: %v", err)
	}
	if string(second) != "efgh" {
		t.Fatalf("second range = %q, want efgh", second)
	}
	secondDeadline, ok := source.ranges[1].ctx.Deadline()
	if !ok || !secondDeadline.After(firstDeadline) {
		t.Fatalf("second range deadline = %v, want after first deadline %v", secondDeadline, firstDeadline)
	}
	if !errors.Is(source.ranges[1].ctx.Err(), context.Canceled) {
		t.Fatalf("second range context error = %v, want canceled after range close", source.ranges[1].ctx.Err())
	}

	last, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read final range: %v", err)
	}
	if string(last) != "ij" {
		t.Fatalf("final range = %q, want ij", last)
	}
	if len(source.ranges) != 3 {
		t.Fatalf("range calls = %d, want 3", len(source.ranges))
	}
	wantOffsets := []int64{0, 4, 8}
	wantLengths := []int64{4, 4, 2}
	for i, call := range source.ranges {
		if call.offset != wantOffsets[i] || call.length != wantLengths[i] {
			t.Fatalf("range %d = offset:%d length:%d, want offset:%d length:%d",
				i, call.offset, call.length, wantOffsets[i], wantLengths[i])
		}
	}
}

func TestSourceObjectRangeReaderEnforcesPerRangeTimeout(t *testing.T) {
	source := blockingSourceObjectReader{size: 4}
	reader, err := newSourceObjectRangeReader(
		context.Background(),
		source,
		"source-bucket",
		"capture.mcap",
		4,
		"blocking-etag",
		4,
		20*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("newSourceObjectRangeReader() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	_, err = io.ReadAll(reader)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadAll() error = %v, want context deadline exceeded", err)
	}
}

type blockingSourceObjectReader struct {
	size int64
}

func (r blockingSourceObjectReader) StatObject(context.Context, string, string) (int64, string, error) {
	return r.size, "blocking-etag", nil
}

func (blockingSourceObjectReader) OpenObjectRange(ctx context.Context, _ string, _ string, _, _, _ int64, _ string) (io.ReadCloser, error) {
	return blockingRangeBody{ctx: ctx}, nil
}

type blockingRangeBody struct {
	ctx context.Context
}

func (b blockingRangeBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (blockingRangeBody) Close() error { return nil }
