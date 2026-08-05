// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package dgwcompat

import "strings"

func buildObjectKey(prefix string, hints map[string]string, uploadID string) string {
	cleanPrefix := strings.Trim(sanitizePathSegment(prefix), "/")
	if cleanPrefix == "" {
		cleanPrefix = "ego-portal-lite"
	}
	if uploadKind(strings.ToLower(strings.TrimSpace(hints["upload_kind"]))) == uploadKindCalibrationCapture {
		return cleanPrefix + "/calibration-captures/" +
			sanitizePathSegment(hints["device_id"]) + "/" +
			sanitizePathSegment(hints["calibration_session_id"]) + "/" +
			sanitizePathSegment(hints["capture_id"]) + "/capture.mcap"
	}
	deviceID := sanitizePathSegment(hints["device_id"])
	if deviceID == "" {
		deviceID = "unknown-device"
	}
	captureID := sanitizePathSegment(hints["capture_id"])
	if captureID == "" {
		captureID = "unknown-capture"
	}
	return cleanPrefix + "/" + deviceID + "/" + captureID + "/" + sanitizePathSegment(uploadID) + "/capture.mcap"
}

func sanitizePathSegment(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "._-")
}
