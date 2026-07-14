import Foundation

import DGWAuth
import DGWProto
import GRPCCore

package struct RetryPolicy: Sendable, Equatable {
    package let maxAttempts: Int
    package let initialBackoff: Duration
    package let maxBackoff: Duration

    package init(
        maxAttempts: Int,
        initialBackoff: Duration,
        maxBackoff: Duration
    ) {
        self.maxAttempts = max(1, maxAttempts)
        self.initialBackoff = initialBackoff
        self.maxBackoff = maxBackoff
    }

    package static let controlPlane = RetryPolicy(
        maxAttempts: 5,
        initialBackoff: Duration(secondsComponent: 0, attosecondsComponent: 500_000_000_000_000_000),
        maxBackoff: .seconds(8)
    )

    package func backoff(forAttempt attempt: Int) -> Duration {
        let exponent = max(0, attempt - 1)
        let scaled = self.initialBackoff.timeInterval * pow(2, Double(exponent))
        return .fromTimeInterval(min(scaled, self.maxBackoff.timeInterval))
    }
}

package enum RetryDecision: Sendable, Equatable {
    case retry
    case refreshAuthorization
    case fail
}

package struct ControlPlaneRetryClassification: Sendable, Equatable {
    package let decision: RetryDecision
    package let statusCode: Int?
    package let detailCode: String?

    package init(decision: RetryDecision, statusCode: Int?, detailCode: String?) {
        self.decision = decision
        self.statusCode = statusCode
        self.detailCode = detailCode
    }
}

package struct ControlPlaneRetryEvent: Sendable, Equatable {
    package enum Action: Sendable, Equatable {
        case retry
        case refreshAuthorization
    }

    package let attempt: Int
    package let action: Action
    package let delay: Duration?
    package let statusCode: Int?
    package let detailCode: String?

    package init(
        attempt: Int,
        action: Action,
        delay: Duration?,
        statusCode: Int?,
        detailCode: String?
    ) {
        self.attempt = attempt
        self.action = action
        self.delay = delay
        self.statusCode = statusCode
        self.detailCode = detailCode
    }
}

package protocol RetrySleeper: Sendable {
    func sleep(for duration: Duration) async throws
}

package struct TaskRetrySleeper: RetrySleeper {
    package init() {}

    package func sleep(for duration: Duration) async throws {
        try await Task.sleep(for: duration)
    }
}

package enum ControlPlaneRetryClassifier {
    package static func classify(_ error: any Error) -> ControlPlaneRetryClassification {
        if let authError = error as? CredentialAuthProviderError {
            return self.classify(authError)
        }

        if let rpcError = error as? RPCError {
            return self.classify(rpcError)
        }

        if let urlError = error as? URLError, self.isRetriableURLFailure(urlError) {
            return ControlPlaneRetryClassification(decision: .retry, statusCode: nil, detailCode: nil)
        }

        return ControlPlaneRetryClassification(decision: .fail, statusCode: nil, detailCode: nil)
    }

    private static func classify(_ error: CredentialAuthProviderError) -> ControlPlaneRetryClassification {
        switch error {
        case .invalidCredential, .invalidResponse:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: nil, detailCode: nil)
        case .authenticationFailed(let code, _):
            if code == "AUTH_INTERNAL_ERROR" {
                return ControlPlaneRetryClassification(decision: .retry, statusCode: RPCError.Code.internalError.rawValue, detailCode: code)
            }
            return ControlPlaneRetryClassification(decision: .fail, statusCode: RPCError.Code.unauthenticated.rawValue, detailCode: code)
        case .transportFailure(let code, _):
            return self.classifyTransportCode(code, detailCode: nil)
        }
    }

    private static func classify(_ error: RPCError) -> ControlPlaneRetryClassification {
        let detailCode = ControlPlaneErrorMapper.decodeErrorDetail(from: error)?.code.nilIfBlank

        switch error.code {
        case .unavailable, .deadlineExceeded:
            return ControlPlaneRetryClassification(decision: .retry, statusCode: error.code.rawValue, detailCode: detailCode)
        case .internalError:
            return ControlPlaneRetryClassification(decision: .retry, statusCode: error.code.rawValue, detailCode: detailCode)
        case .unauthenticated:
            return ControlPlaneRetryClassification(decision: .refreshAuthorization, statusCode: error.code.rawValue, detailCode: detailCode)
        case .invalidArgument, .permissionDenied, .notFound:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: error.code.rawValue, detailCode: detailCode)
        case .failedPrecondition:
            return self.classifyFailedPrecondition(detailCode: detailCode, statusCode: error.code.rawValue)
        case .cancelled:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: error.code.rawValue, detailCode: detailCode)
        default:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: error.code.rawValue, detailCode: detailCode)
        }
    }

    private static func classifyTransportCode(
        _ code: RPCError.Code?,
        detailCode: String?
    ) -> ControlPlaneRetryClassification {
        switch code {
        case .unavailable, .deadlineExceeded, .internalError:
            return ControlPlaneRetryClassification(decision: .retry, statusCode: code?.rawValue, detailCode: detailCode)
        case .unauthenticated:
            return ControlPlaneRetryClassification(decision: .refreshAuthorization, statusCode: code?.rawValue, detailCode: detailCode)
        case .invalidArgument, .permissionDenied, .notFound, .failedPrecondition, .cancelled, .none:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: code?.rawValue, detailCode: detailCode)
        default:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: code?.rawValue, detailCode: detailCode)
        }
    }

    private static func classifyFailedPrecondition(
        detailCode: String?,
        statusCode: Int
    ) -> ControlPlaneRetryClassification {
        switch detailCode {
        case "AUTH_INTERNAL_ERROR":
            return ControlPlaneRetryClassification(decision: .retry, statusCode: statusCode, detailCode: detailCode)
        case "AUTH_INVALID_CREDENTIAL",
            "AUTH_SITE_DISABLED",
            "AUTH_KEY_DISABLED",
            "AUTH_KEY_EXPIRED",
            "DATA_GATEWAY_UPLOAD_NOT_REFRESHABLE",
            "DATA_GATEWAY_FAILED_PRECONDITION":
            return ControlPlaneRetryClassification(decision: .fail, statusCode: statusCode, detailCode: detailCode)
        default:
            return ControlPlaneRetryClassification(decision: .fail, statusCode: statusCode, detailCode: detailCode)
        }
    }

    private static func isRetriableURLFailure(_ error: URLError) -> Bool {
        switch error.code {
        case .timedOut,
            .networkConnectionLost,
            .notConnectedToInternet,
            .cannotConnectToHost,
            .cannotFindHost,
            .dnsLookupFailed:
            return true
        default:
            return false
        }
    }
}

package struct RetryExecutor: Sendable {
    private let sleeper: any RetrySleeper
    private let onEvent: (@Sendable (ControlPlaneRetryEvent) async -> Void)?

    package init(
        sleeper: any RetrySleeper = TaskRetrySleeper(),
        onEvent: (@Sendable (ControlPlaneRetryEvent) async -> Void)? = nil
    ) {
        self.sleeper = sleeper
        self.onEvent = onEvent
    }

    package func execute<T: Sendable>(
        policy: RetryPolicy = .controlPlane,
        retryAuthorizationFailures: Bool = true,
        refreshAuthorization: @Sendable () async throws -> Void = {},
        operation: @Sendable () async throws -> T
    ) async throws -> T {
        var attempt = 1
        var didRefreshAuthorization = false

        while true {
            do {
                return try await operation()
            } catch {
                let classification = ControlPlaneRetryClassifier.classify(error)

                switch classification.decision {
                case .retry where attempt < policy.maxAttempts:
                    let delay = policy.backoff(forAttempt: attempt)
                    if let onEvent {
                        await onEvent(
                            ControlPlaneRetryEvent(
                                attempt: attempt,
                                action: .retry,
                                delay: delay,
                                statusCode: classification.statusCode,
                                detailCode: classification.detailCode
                            )
                        )
                    }
                    try await self.sleeper.sleep(for: delay)
                    attempt += 1
                case .refreshAuthorization where retryAuthorizationFailures && !didRefreshAuthorization && attempt < policy.maxAttempts:
                    if let onEvent {
                        await onEvent(
                            ControlPlaneRetryEvent(
                                attempt: attempt,
                                action: .refreshAuthorization,
                                delay: nil,
                                statusCode: classification.statusCode,
                                detailCode: classification.detailCode
                            )
                        )
                    }
                    didRefreshAuthorization = true
                    try await refreshAuthorization()
                    attempt += 1
                default:
                    throw error
                }
            }
        }
    }
}

package final class AuthenticatedGatewayControlPlaneClient<
    AuthProvider: AuthorizationHeaderProvider,
    GatewayClient: GatewayControlPlaneClientProtocol
>: @unchecked Sendable {
    private let authProvider: AuthProvider
    private let gatewayClient: GatewayClient
    private let retryExecutor: RetryExecutor
    private let retryPolicy: RetryPolicy

    package init(
        authProvider: AuthProvider,
        gatewayClient: GatewayClient,
        retryExecutor: RetryExecutor = RetryExecutor(),
        retryPolicy: RetryPolicy = .controlPlane
    ) {
        self.authProvider = authProvider
        self.gatewayClient = gatewayClient
        self.retryExecutor = retryExecutor
        self.retryPolicy = retryPolicy
    }

    package func createLogicalUpload(
        clientHints: [String: String],
        restartFromUploadID: String? = nil
    ) async throws -> Archebase_DataGateway_V1_CreateLogicalUploadResponse {
        try await self.retryExecutor.execute(policy: self.retryPolicy, refreshAuthorization: self.refreshAuthorization) {
            let header = try await self.authProvider.authorizationHeader()
            return try await self.gatewayClient.createLogicalUpload(
                clientHints: clientHints,
                restartFromUploadID: restartFromUploadID,
                authorizationHeader: header
            )
        }
    }

    package func getUploadRecovery(
        logicalUploadID: String
    ) async throws -> Archebase_DataGateway_V1_GetUploadRecoveryResponse {
        try await self.retryExecutor.execute(policy: self.retryPolicy, refreshAuthorization: self.refreshAuthorization) {
            let header = try await self.authProvider.authorizationHeader()
            return try await self.gatewayClient.getUploadRecovery(
                logicalUploadID: logicalUploadID,
                authorizationHeader: header
            )
        }
    }

    package func reissueUploadCredentials(
        uploadID: String
    ) async throws -> Archebase_DataGateway_V1_ReissueUploadCredentialsResponse {
        try await self.retryExecutor.execute(policy: self.retryPolicy, refreshAuthorization: self.refreshAuthorization) {
            let header = try await self.authProvider.authorizationHeader()
            return try await self.gatewayClient.reissueUploadCredentials(
                uploadID: uploadID,
                authorizationHeader: header
            )
        }
    }

    package func abortUpload(
        logicalUploadID: String,
        reason: String
    ) async throws -> Archebase_DataGateway_V1_AbortUploadResponse {
        try await self.retryExecutor.execute(policy: self.retryPolicy, refreshAuthorization: self.refreshAuthorization) {
            let header = try await self.authProvider.authorizationHeader()
            return try await self.gatewayClient.abortUpload(
                logicalUploadID: logicalUploadID,
                reason: reason,
                authorizationHeader: header
            )
        }
    }

    package func completeUpload(
        uploadID: String,
        fileSize: Int64,
        rawTags: [String: String],
        completedPartCount: Int32,
        ossObjectEtag: String,
        partSizeBytes: Int64
    ) async throws -> Archebase_DataGateway_V1_CompleteUploadResponse {
        try await self.retryExecutor.execute(policy: self.retryPolicy, refreshAuthorization: self.refreshAuthorization) {
            let header = try await self.authProvider.authorizationHeader()
            return try await self.gatewayClient.completeUpload(
                uploadID: uploadID,
                fileSize: fileSize,
                rawTags: rawTags,
                completedPartCount: completedPartCount,
                ossObjectEtag: ossObjectEtag,
                partSizeBytes: partSizeBytes,
                authorizationHeader: header
            )
        }
    }

    private func refreshAuthorization() async throws {
        await self.authProvider.invalidateAuthorization()
    }
}

package final class RetryingObjectControlPlaneClient<
    ObjectClient: ObjectControlPlaneClientProtocol
>: ObjectControlPlaneClientProtocol, @unchecked Sendable {
    private let objectClient: ObjectClient
    private let retryExecutor: RetryExecutor
    private let retryPolicy: RetryPolicy

    package init(
        objectClient: ObjectClient,
        retryExecutor: RetryExecutor = RetryExecutor(),
        retryPolicy: RetryPolicy = .controlPlane
    ) {
        self.objectClient = objectClient
        self.retryExecutor = retryExecutor
        self.retryPolicy = retryPolicy
    }

    package func listObjects(
        pageSize: Int32,
        pageToken: String,
        filter: String,
        authorizationHeader: String
    ) async throws -> Archebase_DataGateway_V1_ListObjectsResponse {
        try await self.retryExecutor.execute(
            policy: self.retryPolicy,
            retryAuthorizationFailures: false
        ) {
            try await self.objectClient.listObjects(
                pageSize: pageSize,
                pageToken: pageToken,
                filter: filter,
                authorizationHeader: authorizationHeader
            )
        }
    }
}

private extension Duration {
    static func fromTimeInterval(_ interval: TimeInterval) -> Duration {
        let clamped = max(0, interval)
        let wholeSeconds = floor(clamped)
        let fractional = clamped - wholeSeconds
        let attoseconds = fractional * 1_000_000_000_000_000_000
        return Duration(
            secondsComponent: Int64(wholeSeconds),
            attosecondsComponent: Int64(attoseconds.rounded())
        )
    }

    var timeInterval: TimeInterval {
        let components = self.components
        return Double(components.seconds) + Double(components.attoseconds) / 1_000_000_000_000_000_000
    }
}
