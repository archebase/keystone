// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

// IsRobotConnected returns whether a robot is considered "connected" in Keystone.
//
// A robot is connected only when both websocket hubs have an active connection:
// - Axon recorder websocket (RecorderHub)
// - Axon transfer websocket (TransferHub)
func IsRobotConnected(
	recorderHub *RecorderHub,
	transferHub *TransferHub,
	deviceID string,
) bool {
	if recorderHub == nil || transferHub == nil || deviceID == "" {
		return false
	}

	recConn := recorderHub.Get(deviceID)
	transConn := transferHub.Get(deviceID)
	if recConn == nil || transConn == nil {
		return false
	}
	return true
}
