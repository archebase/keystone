// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type uploadKind string

const (
	uploadKindTaskEpisode        uploadKind = "task_episode"
	uploadKindCalibrationCapture uploadKind = "calibration_capture"

	maxCalibrationSourceLength        = 64
	maxCalibrationLocalOperatorLength = 255
	maxCameraSerialLength             = 255
)

type uploadIntent struct {
	Kind                 uploadKind
	CameraSerial         string
	CalibrationSessionID string
	CaptureID            string
	AttemptNo            int64
	ChecksumSHA256       string
}

func parseUploadIntent(hints map[string]string) (uploadIntent, error) {
	kind := uploadKind(strings.ToLower(strings.TrimSpace(hints["upload_kind"])))
	if kind == "" {
		kind = uploadKindTaskEpisode
	}
	hints["upload_kind"] = string(kind)

	switch kind {
	case uploadKindTaskEpisode:
		cameraSerial := ""
		if strings.TrimSpace(hints["camera_serial"]) != "" {
			var err error
			cameraSerial, err = normalizeCameraSerial(hints["camera_serial"])
			if err != nil {
				return uploadIntent{}, err
			}
		}
		hints["camera_serial"] = cameraSerial
		return uploadIntent{
			Kind:         kind,
			CameraSerial: cameraSerial,
			CaptureID:    strings.TrimSpace(hints["capture_id"]),
		}, nil
	case uploadKindCalibrationCapture:
		cameraSerial, err := normalizeCameraSerial(hints["camera_serial"])
		if err != nil {
			return uploadIntent{}, err
		}
		sessionID, err := parseCanonicalV4UUID(hints["calibration_session_id"])
		if err != nil {
			return uploadIntent{}, status.Error(codes.InvalidArgument, "calibration_session_id must be a canonical UUIDv4")
		}
		captureID, err := parseCanonicalV4UUID(hints["capture_id"])
		if err != nil {
			return uploadIntent{}, status.Error(codes.InvalidArgument, "capture_id must be a canonical UUIDv4")
		}
		attemptNo, err := parsePositiveInt64Hint(hints, "attempt_no")
		if err != nil {
			return uploadIntent{}, err
		}
		checksum := strings.ToLower(strings.TrimSpace(hints["checksum_sha256"]))
		if !isSHA256Hex(checksum) {
			return uploadIntent{}, status.Error(codes.InvalidArgument, "checksum_sha256 must be a 64-character hexadecimal SHA-256 digest")
		}
		if err := normalizeBoundedOptionalHint(hints, "source", maxCalibrationSourceLength); err != nil {
			return uploadIntent{}, err
		}
		if err := normalizeBoundedOptionalHint(hints, "local_operator", maxCalibrationLocalOperatorLength); err != nil {
			return uploadIntent{}, err
		}
		hints["calibration_session_id"] = sessionID
		hints["camera_serial"] = cameraSerial
		hints["capture_id"] = captureID
		hints["attempt_no"] = strconv.FormatInt(attemptNo, 10)
		hints["checksum_sha256"] = checksum
		return uploadIntent{
			Kind:                 kind,
			CameraSerial:         cameraSerial,
			CalibrationSessionID: sessionID,
			CaptureID:            captureID,
			AttemptNo:            attemptNo,
			ChecksumSHA256:       checksum,
		}, nil
	default:
		return uploadIntent{}, status.Errorf(codes.InvalidArgument, "unsupported upload_kind %q", kind)
	}
}

func normalizeCameraSerial(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", status.Error(codes.InvalidArgument, "camera_serial is required")
	}
	if utf8.RuneCountInString(value) > maxCameraSerialLength {
		return "", status.Errorf(codes.InvalidArgument, "camera_serial must not exceed %d characters", maxCameraSerialLength)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", status.Error(codes.InvalidArgument, "camera_serial must not contain control characters")
		}
	}
	return value, nil
}

func normalizeBoundedOptionalHint(hints map[string]string, key string, maxLength int) error {
	value := strings.TrimSpace(hints[key])
	if utf8.RuneCountInString(value) > maxLength {
		return status.Errorf(codes.InvalidArgument, "%s must not exceed %d characters", key, maxLength)
	}
	hints[key] = value
	return nil
}

func parseCanonicalV4UUID(raw string) (string, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || parsed.String() != raw {
		return "", fmt.Errorf("not a canonical UUIDv4")
	}
	return parsed.String(), nil
}
