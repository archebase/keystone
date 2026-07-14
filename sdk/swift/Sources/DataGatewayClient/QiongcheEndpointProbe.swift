import DGWControlPlane
import DGWProto
import Foundation
import GRPCCore

package protocol QiongcheEndpointProbing: Sendable {
    func authEndpointReachable(endpoint: URL, tls: TLSMode, timeout: Duration) async -> Bool
    func gatewayEndpointReachable(endpoint: URL, tls: TLSMode, timeout: Duration) async -> Bool
}

package enum QiongcheEndpointReachability {
    package static func isReachable(error: any Error) -> Bool {
        if error is QiongcheProbeTimeoutError {
            return false
        }

        if let rpcError = error as? RPCError {
            return self.isReachable(rpcCode: rpcError.code)
        }

        if error is CancellationError {
            return false
        }

        return false
    }

    package static func isReachable(rpcCode: RPCError.Code) -> Bool {
        switch rpcCode {
        case .unauthenticated, .permissionDenied, .invalidArgument, .notFound, .failedPrecondition:
            return true
        case .unavailable, .deadlineExceeded, .cancelled:
            return false
        default:
            return false
        }
    }
}

package struct QiongcheProbeTimeoutError: Error, Sendable, Equatable {}

package enum QiongcheProbeTimeout {
    package static func run<T: Sendable>(
        timeout: Duration,
        operation: @Sendable @escaping () async throws -> T
    ) async throws -> T {
        try await withThrowingTaskGroup(of: T.self) { group in
            group.addTask {
                try await operation()
            }
            group.addTask {
                try await Task.sleep(for: timeout)
                throw QiongcheProbeTimeoutError()
            }

            defer {
                group.cancelAll()
            }
            guard let result = try await group.next() else {
                throw QiongcheProbeTimeoutError()
            }
            return result
        }
    }
}

package struct DefaultQiongcheEndpointProbe: QiongcheEndpointProbing {
    private static let probeCredentialBase64 = "qiongche-readiness-probe"
    private static let probeLogicalUploadID = "qiongche-readiness-probe"

    package init() {}

    package func authEndpointReachable(endpoint: URL, tls: TLSMode, timeout: Duration) async -> Bool {
        do {
            try DataGatewayClientConfig.validate(endpoint: endpoint, tls: tls, fieldName: "authEndpoint")
            let managedTransport = try self.makeFactory(endpoint: endpoint, tls: tls, timeout: timeout)
                .makeAuthTransport()
            defer {
                managedTransport.shutdown()
            }

            _ = try await QiongcheProbeTimeout.run(timeout: timeout) {
                try await managedTransport.serviceClient.exchangeCredential(
                    credentialBase64: Self.probeCredentialBase64,
                    timeout: timeout
                )
            }
            return true
        } catch {
            return QiongcheEndpointReachability.isReachable(error: error)
        }
    }

    package func gatewayEndpointReachable(endpoint: URL, tls: TLSMode, timeout: Duration) async -> Bool {
        do {
            try DataGatewayClientConfig.validate(endpoint: endpoint, tls: tls, fieldName: "gatewayEndpoint")
            let managedTransport = try self.makeFactory(endpoint: endpoint, tls: tls, timeout: timeout)
                .makeGatewayClient()
            defer {
                managedTransport.shutdown()
            }

            var request = Archebase_DataGateway_V1_GetUploadRecoveryRequest()
            request.logicalUploadID = Self.probeLogicalUploadID
            var options = CallOptions.defaults
            options.timeout = timeout
            let probeRequest = request
            let callOptions = options

            _ = try await QiongcheProbeTimeout.run(timeout: timeout) {
                try await managedTransport.serviceClient.getUploadRecovery(
                    probeRequest,
                    metadata: Metadata(),
                    options: callOptions
                )
            }
            return true
        } catch {
            return QiongcheEndpointReachability.isReachable(error: error)
        }
    }

    private func makeFactory(endpoint: URL, tls: TLSMode, timeout: Duration) -> ControlPlaneClientFactory {
        let security: ControlPlaneTransportSecurity = switch tls {
        case .plaintext: .plaintext
        case .tls: .tls
        }
        return ControlPlaneClientFactory(
            configuration: ControlPlaneTransportConfiguration(
                endpoint: endpoint,
                security: security,
                requestTimeout: timeout
            )
        )
    }
}
