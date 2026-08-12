// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package stereosplit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"

	orbitapi "archebase.com/keystone-edge/internal/orbit"
	"archebase.com/keystone-edge/internal/storage/objectrange"
)

const (
	bytesPerGiB                 int64 = 1 << 30
	scratchSourceSizeMultiplier int64 = 3
	scratchStorageReserveGiB    int64 = 1
	scratchStorageMinRequestGiB int64 = 4
	scratchStorageLimitGiB      int64 = 100
)

// ErrOutputNotSettled marks output reads that may succeed on a later poll.
var ErrOutputNotSettled = errors.New("stereo split output is not settled")

var errScratchStorageExceeded = errors.New("stereo split scratch storage limit exceeded")

// ExecutionInput identifies one TOS MCAP and the durable Orbit execution namespace.
type ExecutionInput struct {
	SourceBucket    string
	SourceObjectKey string
	SourceChecksum  string
	OutputScope     string
	SubmissionID    string
	Generation      int
	Calibration     *CalibrationInput
}

// CalibrationInput identifies one immutable successful calibration result.
type CalibrationInput struct {
	CameraSerial    string
	SessionID       string
	CaptureID       string
	ResultBucket    string
	ResultObjectKey string
	ResultSHA256    string
}

// CalibrationSnapshot freezes one result object into a reusable Orbit request.
type CalibrationSnapshot struct {
	CameraSerial    string `json:"camera_serial"`
	SessionID       string `json:"session_id"`
	CaptureID       string `json:"capture_id"`
	ResultURI       string `json:"result_uri"`
	ResultETag      string `json:"result_etag"`
	ResultSizeBytes int64  `json:"result_size_bytes"`
	ResultSHA256    string `json:"result_sha256"`
}

// ExecutionSnapshot freezes one reusable stereo-split Orbit execution.
type ExecutionSnapshot struct {
	Generation                int                    `json:"generation"`
	ProcessorConfigRevisionID int64                  `json:"processor_config_revision_id"`
	ProcessorImage            string                 `json:"processor_image"`
	SourceURI                 string                 `json:"source_uri"`
	SourceETag                string                 `json:"source_etag"`
	SourceChecksum            string                 `json:"source_checksum,omitempty"`
	SourceSizeBytes           int64                  `json:"source_size_bytes"`
	Calibration               *CalibrationSnapshot   `json:"calibration,omitempty"`
	OutputBucket              string                 `json:"output_bucket"`
	OutputPrefix              string                 `json:"output_prefix"`
	Request                   orbitapi.SubmitRequest `json:"request"`
}

// VerifiedOutput is the fixed, validated output of one stereo-split execution.
type VerifiedOutput struct {
	MCAPObjectKey         string  `json:"mcap_object_key"`
	MCAPSizeBytes         int64   `json:"mcap_size_bytes"`
	MCAPChecksumSHA256    string  `json:"mcap_checksum_sha256"`
	MCAPETag              string  `json:"mcap_etag"`
	MetadataObjectKey     string  `json:"metadata_object_key"`
	ManifestObjectKey     string  `json:"manifest_object_key"`
	ManifestJSON          string  `json:"manifest_json"`
	ProcessingDurationSec float64 `json:"processing_duration_sec"`
}

// PrepareExecution validates and freezes the fixed stereo-split Job contract.
func (m *Manager) PrepareExecution(ctx context.Context, input ExecutionInput) (ExecutionSnapshot, error) {
	if m == nil || m.db == nil {
		return ExecutionSnapshot{}, fmt.Errorf("prepare stereo split: database is not configured")
	}
	if !m.cfg.Enabled {
		return ExecutionSnapshot{}, ErrDisabled
	}
	if m.objects == nil {
		return ExecutionSnapshot{}, fmt.Errorf("prepare stereo split: TOS object reader is not configured")
	}
	currentImage, err := m.CurrentImageConfig(ctx)
	if err != nil {
		return ExecutionSnapshot{}, err
	}
	if currentImage.ImageRef == "" {
		return ExecutionSnapshot{}, ErrImageNotConfigured
	}
	imageRef, err := validateImageRef(currentImage.ImageRef)
	if err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("current stereo split image is invalid: %w", err)
	}

	bucket := strings.TrimSpace(input.SourceBucket)
	objectKey := strings.Trim(strings.TrimSpace(input.SourceObjectKey), "/")
	outputScope := strings.Trim(strings.TrimSpace(input.OutputScope), "/")
	submissionID := strings.TrimSpace(input.SubmissionID)
	if bucket == "" || objectKey == "" || outputScope == "" || submissionID == "" || input.Generation <= 0 ||
		path.Clean(outputScope) != outputScope || outputScope == "." || strings.HasPrefix(outputScope, "../") {
		return ExecutionSnapshot{}, fmt.Errorf("invalid reusable stereo split execution identity")
	}
	sourceSize, sourceETag, err := m.objects.StatObject(ctx, bucket, objectKey)
	if err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("stat stereo split source: %w", err)
	}
	if sourceSize <= 0 || (m.cfg.MaxSourceBytes > 0 && sourceSize > m.cfg.MaxSourceBytes) {
		return ExecutionSnapshot{}, fmt.Errorf("%w: source size %d exceeds allowed range", ErrSourceUnavailable, sourceSize)
	}
	sourceETag = strings.TrimSpace(sourceETag)
	if sourceETag == "" {
		return ExecutionSnapshot{}, fmt.Errorf("%w: source ETag is missing", ErrSourceUnavailable)
	}
	scratchRequest, err := scratchStorageRequest(sourceSize)
	if err != nil {
		return ExecutionSnapshot{}, err
	}
	randomSuffix, err := randomOutputSuffix()
	if err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("generate stereo split output prefix: %w", err)
	}
	outputPrefix := path.Join(
		strings.Trim(m.cfg.OutputPrefix, "/"),
		outputScope,
		fmt.Sprintf("g%d-%s", input.Generation, randomSuffix),
	)
	sourceURI := "tos://" + bucket + "/" + objectKey
	outputURI := "tos://" + strings.TrimSpace(m.cfg.OutputBucket) + "/" + outputPrefix + "/"
	inputPath := "/bindings/input/" + path.Base(objectKey)
	checksum := normalizedSHA256(input.SourceChecksum)
	var calibration *CalibrationSnapshot
	if input.Calibration != nil {
		cameraSerial := strings.TrimSpace(input.Calibration.CameraSerial)
		sessionID := strings.TrimSpace(input.Calibration.SessionID)
		captureID := strings.TrimSpace(input.Calibration.CaptureID)
		resultBucket := strings.TrimSpace(input.Calibration.ResultBucket)
		resultKey := strings.Trim(strings.TrimSpace(input.Calibration.ResultObjectKey), "/")
		resultChecksum := normalizedSHA256(input.Calibration.ResultSHA256)
		if cameraSerial == "" || sessionID == "" || captureID == "" || resultBucket == "" ||
			resultKey == "" || resultChecksum == "" || path.Clean(resultKey) != resultKey ||
			strings.HasPrefix(resultKey, "../") {
			return ExecutionSnapshot{}, fmt.Errorf("invalid calibration result identity")
		}
		resultSize, resultETag, err := m.objects.StatObject(ctx, resultBucket, resultKey)
		if err != nil {
			return ExecutionSnapshot{}, fmt.Errorf("stat calibration result: %w", err)
		}
		resultETag = strings.TrimSpace(resultETag)
		if resultSize <= 0 || resultETag == "" {
			return ExecutionSnapshot{}, fmt.Errorf("calibration result identity is incomplete")
		}
		calibration = &CalibrationSnapshot{
			CameraSerial:    cameraSerial,
			SessionID:       sessionID,
			CaptureID:       captureID,
			ResultURI:       "tos://" + resultBucket + "/" + resultKey,
			ResultETag:      resultETag,
			ResultSizeBytes: resultSize,
			ResultSHA256:    resultChecksum,
		}
	}
	backoffLimit := int32(0)
	ttlSeconds := m.cfg.TTLSecondsAfterDone
	deadline := m.cfg.ActiveDeadline
	resourceRequests := cloneStringMap(m.cfg.Resources.Requests)
	if resourceRequests == nil {
		resourceRequests = make(map[string]string)
	}
	resourceLimits := cloneStringMap(m.cfg.Resources.Limits)
	if resourceLimits == nil {
		resourceLimits = make(map[string]string)
	}
	resourceRequests["ephemeral-storage"] = scratchRequest
	resourceLimits["ephemeral-storage"] = fmt.Sprintf("%dGi", scratchStorageLimitGiB)
	request := orbitapi.SubmitRequest{
		SubmissionID: submissionID,
		Image:        imageRef,
		Command:      []string{"python3", processingCommand},
		Args: []string{
			"--input", inputPath,
			"--output-binding", "/bindings/output",
			"--scratch", "/scratch",
			"--expected-source-size", strconv.FormatInt(sourceSize, 10),
			"--expected-source-checksum", checksum,
			"--source-uri", sourceURI,
			"--processor-image", imageRef,
			"--kind", Kind,
			"--generation", strconv.Itoa(input.Generation),
		},
		DataBindings: []orbitapi.DataBinding{
			{URI: sourceURI, Path: inputPath, Mode: "read"},
		},
		Resources: orbitapi.Resources{
			Requests: resourceRequests,
			Limits:   resourceLimits,
		},
		TTLSecondsAfterDone:  &ttlSeconds,
		BackoffLimit:         &backoffLimit,
		ActiveDeadlineSecond: &deadline,
	}
	if calibration != nil {
		const calibrationPath = "/bindings/calibration/calibration.json"
		request.Args = append(request.Args,
			"--calibration-result", calibrationPath,
			"--calibration-camera-serial", calibration.CameraSerial,
			"--calibration-session-id", calibration.SessionID,
			"--calibration-capture-id", calibration.CaptureID,
			"--expected-calibration-size", strconv.FormatInt(calibration.ResultSizeBytes, 10),
			"--expected-calibration-checksum", calibration.ResultSHA256,
		)
		request.DataBindings = append(request.DataBindings, orbitapi.DataBinding{
			URI: calibration.ResultURI, Path: calibrationPath, Mode: "read",
		})
	}
	request.DataBindings = append(request.DataBindings, orbitapi.DataBinding{
		URI: outputURI, Path: "/bindings/output/", Mode: "write",
	})
	return ExecutionSnapshot{
		Generation:                input.Generation,
		ProcessorConfigRevisionID: currentImage.ID,
		ProcessorImage:            imageRef,
		SourceURI:                 sourceURI,
		SourceETag:                sourceETag,
		SourceChecksum:            checksum,
		SourceSizeBytes:           sourceSize,
		Calibration:               calibration,
		OutputBucket:              strings.TrimSpace(m.cfg.OutputBucket),
		OutputPrefix:              outputPrefix,
		Request:                   request,
	}, nil
}

func scratchStorageRequest(sourceSizeBytes int64) (string, error) {
	reserveBytes := scratchStorageReserveGiB * bytesPerGiB
	if sourceSizeBytes > (math.MaxInt64-reserveBytes)/scratchSourceSizeMultiplier {
		return "", fmt.Errorf(
			"%w: %w: source size %d cannot be represented safely",
			ErrSourceUnavailable,
			errScratchStorageExceeded,
			sourceSizeBytes,
		)
	}
	requiredBytes := sourceSizeBytes*scratchSourceSizeMultiplier + reserveBytes
	requiredGiB := requiredBytes / bytesPerGiB
	if requiredBytes%bytesPerGiB != 0 {
		requiredGiB++
	}
	if requiredGiB < scratchStorageMinRequestGiB {
		requiredGiB = scratchStorageMinRequestGiB
	}
	if requiredGiB > scratchStorageLimitGiB {
		return "", fmt.Errorf(
			"%w: %w: stereo split source requires %dGi ephemeral storage, maximum is %dGi",
			ErrSourceUnavailable,
			errScratchStorageExceeded,
			requiredGiB,
			scratchStorageLimitGiB,
		)
	}
	return fmt.Sprintf("%dGi", requiredGiB), nil
}

// VerifyExecution validates the frozen source, manifest and fixed output objects.
func (m *Manager) VerifyExecution(ctx context.Context, execution ExecutionSnapshot) (VerifiedOutput, error) {
	if m == nil || m.objects == nil {
		return VerifiedOutput{}, fmt.Errorf("verify stereo split: TOS object reader is not configured")
	}
	if objectrange.NormalizeETag(execution.SourceETag) == "" {
		return VerifiedOutput{}, fmt.Errorf("frozen source ETag is empty")
	}
	sourceBucket, sourceKey, err := parseFrozenTOSURI(execution.SourceURI)
	if err != nil {
		return VerifiedOutput{}, err
	}
	sourceSize, sourceETag, err := m.objects.StatObject(ctx, sourceBucket, sourceKey)
	if err != nil {
		return VerifiedOutput{}, fmt.Errorf("%w: stat frozen source object: %v", ErrOutputNotSettled, err)
	}
	if sourceSize != execution.SourceSizeBytes ||
		objectrange.NormalizeETag(sourceETag) != objectrange.NormalizeETag(execution.SourceETag) {
		return VerifiedOutput{}, fmt.Errorf("source object identity changed after execution snapshot was frozen")
	}
	if err := m.verifyFrozenCalibrationResult(ctx, execution.Calibration); err != nil {
		return VerifiedOutput{}, err
	}

	manifestKey := path.Join(execution.OutputPrefix, manifestName)
	body, err := m.objects.OpenObject(ctx, execution.OutputBucket, manifestKey)
	if err != nil {
		return VerifiedOutput{}, fmt.Errorf("%w: open processing manifest: %v", ErrOutputNotSettled, err)
	}
	manifestBytes, readErr := io.ReadAll(io.LimitReader(body, maxManifestBytes+1))
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		return VerifiedOutput{}, fmt.Errorf("%w: read processing manifest: read=%v close=%v", ErrOutputNotSettled, readErr, closeErr)
	}
	if len(manifestBytes) > maxManifestBytes {
		return VerifiedOutput{}, fmt.Errorf("processing manifest exceeds %d bytes", maxManifestBytes)
	}
	var manifest processingManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return VerifiedOutput{}, fmt.Errorf("decode processing manifest: %w", err)
	}
	if err := validateManifestSnapshot(manifest, execution); err != nil {
		return VerifiedOutput{}, err
	}
	mcapKey := path.Join(execution.OutputPrefix, outputMcapName)
	metadataKey := path.Join(execution.OutputPrefix, outputMetadataName)
	mcapETag, err := m.verifyExecutionOutputObject(ctx, execution.OutputBucket, mcapKey, manifest.Outputs.MCAP, true)
	if err != nil {
		return VerifiedOutput{}, err
	}
	if _, err := m.verifyExecutionOutputObject(ctx, execution.OutputBucket, metadataKey, manifest.Outputs.Metadata, false); err != nil {
		return VerifiedOutput{}, err
	}
	duration := manifest.FinishedAt.Sub(manifest.StartedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	return VerifiedOutput{
		MCAPObjectKey:         mcapKey,
		MCAPSizeBytes:         manifest.Outputs.MCAP.SizeBytes,
		MCAPChecksumSHA256:    manifest.Outputs.MCAP.SHA256,
		MCAPETag:              mcapETag,
		MetadataObjectKey:     metadataKey,
		ManifestObjectKey:     manifestKey,
		ManifestJSON:          string(manifestBytes),
		ProcessingDurationSec: duration,
	}, nil
}

func (m *Manager) verifyFrozenCalibrationResult(ctx context.Context, calibration *CalibrationSnapshot) error {
	if calibration == nil {
		return nil
	}
	if strings.TrimSpace(calibration.CameraSerial) == "" || strings.TrimSpace(calibration.SessionID) == "" ||
		strings.TrimSpace(calibration.CaptureID) == "" || calibration.ResultSizeBytes <= 0 ||
		objectrange.NormalizeETag(calibration.ResultETag) == "" ||
		normalizedSHA256(calibration.ResultSHA256) == "" {
		return fmt.Errorf("frozen calibration result identity is incomplete")
	}
	bucket, objectKey, err := parseFrozenTOSURI(calibration.ResultURI)
	if err != nil {
		return fmt.Errorf("invalid frozen calibration result URI: %w", err)
	}
	size, etag, err := m.objects.StatObject(ctx, bucket, objectKey)
	if err != nil {
		return fmt.Errorf("%w: stat frozen calibration result: %w", ErrOutputNotSettled, err)
	}
	if size != calibration.ResultSizeBytes ||
		objectrange.NormalizeETag(etag) != objectrange.NormalizeETag(calibration.ResultETag) {
		return fmt.Errorf("calibration result identity changed after execution snapshot was frozen")
	}
	return nil
}

func (m *Manager) verifyExecutionOutputObject(
	ctx context.Context,
	bucket string,
	objectKey string,
	output manifestOutput,
	checkMagic bool,
) (string, error) {
	size, etag, err := m.objects.StatObject(ctx, bucket, objectKey)
	if err != nil {
		return "", fmt.Errorf("%w: stat output object %s: %v", ErrOutputNotSettled, objectKey, err)
	}
	if size != output.SizeBytes || size <= 0 || strings.TrimSpace(etag) == "" {
		return "", fmt.Errorf("%w: output object %s identity does not match manifest", ErrOutputNotSettled, objectKey)
	}
	if !checkMagic {
		return strings.TrimSpace(etag), nil
	}
	if size < int64(len(mcapMagic)*2) {
		return "", fmt.Errorf("output MCAP is too small")
	}
	for _, offset := range []int64{0, size - int64(len(mcapMagic))} {
		body, err := m.objects.OpenObjectRange(ctx, bucket, objectKey, offset, int64(len(mcapMagic)), size, etag)
		if err != nil {
			return "", fmt.Errorf("%w: read output MCAP magic at %d: %v", ErrOutputNotSettled, offset, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(body, int64(len(mcapMagic)+1)))
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			return "", fmt.Errorf("%w: read output MCAP magic at %d: read=%v close=%v", ErrOutputNotSettled, offset, readErr, closeErr)
		}
		if !bytes.Equal(data, mcapMagic) {
			return "", fmt.Errorf("output MCAP has invalid magic at %d", offset)
		}
	}
	return strings.TrimSpace(etag), nil
}
