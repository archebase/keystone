// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import DGWControlPlane
import DGWStore
import Foundation

package protocol QiongcheDeviceProvisioning: Sendable {
    func initDevice(
        deviceName: String,
        deviceAuthToken: String,
        deviceInitEndpoint: URL,
        tls: TLSMode,
        timeout: Duration
    ) async throws -> ArchebaseConfig
}

package struct QiongcheDeviceInitTransportHandle: Sendable {
    package let serviceClient: any DeviceInitTransport
    private let shutdownHandler: @Sendable () -> Void

    package init(
        serviceClient: any DeviceInitTransport,
        shutdown: @escaping @Sendable () -> Void
    ) {
        self.serviceClient = serviceClient
        self.shutdownHandler = shutdown
    }

    package func shutdown() {
        self.shutdownHandler()
    }
}

package struct DefaultQiongcheDeviceProvisioner: QiongcheDeviceProvisioning {
    private let makeTransport: @Sendable (URL, TLSMode, Duration) throws -> QiongcheDeviceInitTransportHandle

    package init(
        makeTransport: @escaping @Sendable (URL, TLSMode, Duration) throws -> QiongcheDeviceInitTransportHandle = Self.makeDefaultTransport
    ) {
        self.makeTransport = makeTransport
    }

    package func initDevice(
        deviceName: String,
        deviceAuthToken: String,
        deviceInitEndpoint: URL,
        tls: TLSMode,
        timeout: Duration
    ) async throws -> ArchebaseConfig {
        try DataGatewayClientConfig.validate(endpoint: deviceInitEndpoint, tls: tls, fieldName: "deviceInitEndpoint")

        let transport = try self.makeTransport(deviceInitEndpoint, tls, timeout)
        defer {
            transport.shutdown()
        }

        return try await DeviceInitConfigFetcher.fetch(
            mode: .initDevice,
            deviceName: deviceName,
            deviceAuthToken: deviceAuthToken,
            authorizationHeader: nil,
            transport: transport.serviceClient,
            sdkVersion: DataGatewayClientModule.version,
            platform: "ios"
        )
    }

    private static func makeDefaultTransport(
        deviceInitEndpoint: URL,
        tls: TLSMode,
        timeout: Duration
    ) throws -> QiongcheDeviceInitTransportHandle {
        let security: ControlPlaneTransportSecurity = switch tls {
        case .plaintext: .plaintext
        case .tls: .tls
        }
        let factory = ControlPlaneClientFactory(
            configuration: ControlPlaneTransportConfiguration(
                endpoint: deviceInitEndpoint,
                security: security,
                requestTimeout: timeout
            )
        )
        let managedTransport = try factory.makeDeviceInitTransport()
        return QiongcheDeviceInitTransportHandle(
            serviceClient: managedTransport.serviceClient,
            shutdown: {
                managedTransport.shutdown()
            }
        )
    }
}
