// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import DGWControlPlane
import DGWStore
import Foundation

package enum DeviceInitRemoteMode: Sendable {
    case initDevice
    case reinitDevice
}

package enum DeviceInitConfigFetcher {
    package static func fetch(
        mode: DeviceInitRemoteMode,
        deviceID: String,
        deviceAuthToken: String?,
        authorizationHeader: String? = nil,
        transport: any DeviceInitTransport,
        sdkVersion: String,
        platform: String
    ) async throws -> ArchebaseConfig {
        do {
            let response = switch mode {
            case .initDevice:
                guard let deviceAuthToken = deviceAuthToken?.trimmingCharacters(in: .whitespacesAndNewlines), !deviceAuthToken.isEmpty else {
                    throw DataGatewayClientError.invalidConfiguration("device_auth_token is required for device initialization")
                }
                try await transport.initDevice(
                    deviceID: deviceID,
                    deviceAuthToken: deviceAuthToken,
                    sdkVersion: sdkVersion,
                    platform: platform
                )
            case .reinitDevice:
                try await transport.reinitDevice(
                    deviceID: deviceID,
                    deviceAuthToken: deviceAuthToken ?? "",
                    authorizationHeader: authorizationHeader,
                    sdkVersion: sdkVersion,
                    platform: platform
                )
            }
            return try ArchebaseConfig(apiKey: response.apiKey, tags: response.tags)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw ControlPlaneErrorMapper.map(error)
        }
    }
}
