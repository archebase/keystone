// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const defaultSourceObjectRangeSize int64 = 64 * 1024 * 1024

// sourceObjectRangeReader exposes one logical object stream while fetching it
// through bounded Range GETs. Each range receives a fresh timeout and is closed
// before the next range starts, so time spent uploading a completed part does
// not consume the following source-read deadline.
type sourceObjectRangeReader struct {
	ctx        context.Context
	source     SourceObjectReader
	bucket     string
	objectName string
	objectSize int64
	objectETag string
	rangeSize  int64
	timeout    time.Duration

	offset          int64
	activeStart     int64
	activeRemaining int64
	activeBody      io.ReadCloser
	activeCancel    context.CancelFunc
	closed          bool
}

func newSourceObjectRangeReader(
	ctx context.Context,
	source SourceObjectReader,
	bucket string,
	objectName string,
	objectSize int64,
	objectETag string,
	rangeSize int64,
	timeout time.Duration,
) (*sourceObjectRangeReader, error) {
	if source == nil {
		return nil, fmt.Errorf("source object reader is not configured")
	}
	if objectSize <= 0 {
		return nil, fmt.Errorf("source object size must be positive")
	}
	if objectETag == "" {
		return nil, fmt.Errorf("source object ETag must not be empty")
	}
	if rangeSize <= 0 {
		return nil, fmt.Errorf("source object range size must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &sourceObjectRangeReader{
		ctx:        ctx,
		source:     source,
		bucket:     bucket,
		objectName: objectName,
		objectSize: objectSize,
		objectETag: objectETag,
		rangeSize:  rangeSize,
		timeout:    timeout,
	}, nil
}

func (r *sourceObjectRangeReader) Read(p []byte) (int, error) {
	if r == nil || r.closed {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset >= r.objectSize {
		return 0, io.EOF
	}
	if r.activeBody == nil {
		if err := r.openNextRange(); err != nil {
			return 0, err
		}
	}

	readSize := min(int64(len(p)), r.activeRemaining)
	n, err := r.activeBody.Read(p[:int(readSize)])
	if n < 0 || int64(n) > readSize {
		_ = r.closeActiveRange()
		return 0, fmt.Errorf("source object range returned invalid read size %d", n)
	}
	r.offset += int64(n)
	r.activeRemaining -= int64(n)

	if r.activeRemaining == 0 {
		closeErr := r.closeActiveRange()
		if err != nil && !errors.Is(err, io.EOF) {
			return n, r.wrapActiveReadError(err)
		}
		if closeErr != nil {
			return n, fmt.Errorf("close source object range offset=%d: %w", r.activeStart, closeErr)
		}
		return n, nil
	}

	if err != nil {
		_ = r.closeActiveRange()
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return n, r.wrapActiveReadError(err)
	}
	if n == 0 {
		_ = r.closeActiveRange()
		return 0, r.wrapActiveReadError(io.ErrNoProgress)
	}
	return n, nil
}

func (r *sourceObjectRangeReader) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	return r.closeActiveRange()
}

func (r *sourceObjectRangeReader) openNextRange() error {
	length := min(r.rangeSize, r.objectSize-r.offset)
	var rangeCtx context.Context
	var cancel context.CancelFunc
	if r.timeout > 0 {
		rangeCtx, cancel = context.WithTimeout(r.ctx, r.timeout)
	} else {
		rangeCtx, cancel = context.WithCancel(r.ctx)
	}
	body, err := r.source.OpenObjectRange(rangeCtx, r.bucket, r.objectName, r.offset, length, r.objectSize, r.objectETag)
	if err != nil {
		cancel()
		return fmt.Errorf("open source object range offset=%d size=%d: %w", r.offset, length, err)
	}
	if body == nil {
		cancel()
		return fmt.Errorf("open source object range offset=%d size=%d: empty body", r.offset, length)
	}
	r.activeStart = r.offset
	r.activeRemaining = length
	r.activeBody = body
	r.activeCancel = cancel
	return nil
}

func (r *sourceObjectRangeReader) closeActiveRange() error {
	body := r.activeBody
	cancel := r.activeCancel
	r.activeBody = nil
	r.activeCancel = nil
	r.activeRemaining = 0
	var err error
	if body != nil {
		err = body.Close()
	}
	if cancel != nil {
		cancel()
	}
	return err
}

func (r *sourceObjectRangeReader) wrapActiveReadError(err error) error {
	return fmt.Errorf("read source object range offset=%d: %w", r.activeStart, err)
}
