// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// DeviceAuthTokenVersion identifies administrator-issued device credentials.
const DeviceAuthTokenVersion = "kda_v1"

// GenerateDeviceAuthToken returns a plaintext token that is only exposed at issuance time.
func GenerateDeviceAuthToken() (string, error) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return DeviceAuthTokenVersion + "_" + base64.RawURLEncoding.EncodeToString(randomBytes), nil
}

// HashDeviceAuthToken returns the database representation of a plaintext device token.
func HashDeviceAuthToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
