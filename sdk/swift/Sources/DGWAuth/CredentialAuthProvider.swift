import Foundation

import DGWProto
import GRPCCore

private let authStatusDetailsMetadataKey = "grpc-status-details-bin"

package protocol CredentialExchangeTransport: Sendable {
    func exchangeCredential(
        credentialBase64: String,
        timeout: Duration
    ) async throws -> Archebase_Auth_V1_ExchangeCredentialResponse
}

package protocol CredentialAuthProviderClock: Sendable {
    func now() async -> Date
}

package protocol AuthorizationHeaderProvider: Sendable {
    func authorizationHeader() async throws -> String
    func invalidateAuthorization() async
}

package struct SystemCredentialAuthProviderClock: CredentialAuthProviderClock {
    package init() {}

    package func now() async -> Date {
        Date()
    }
}

package struct AuthAccessToken: Sendable, Equatable {
    package let accessToken: String
    package let expiresAt: Date

    package init(accessToken: String, expiresAt: Date) {
        self.accessToken = accessToken
        self.expiresAt = expiresAt
    }

    package var authorizationHeader: String {
        "Bearer \(self.accessToken)"
    }
}

package enum CredentialAuthProviderError: Error, Sendable, Equatable {
    case invalidCredential(String)
    case invalidResponse(String)
    case authenticationFailed(code: String?, message: String)
    case transportFailure(code: RPCError.Code?, message: String)
}

package struct AuthServiceClientTransport<Client: Archebase_Auth_V1_AuthService.ClientProtocol>: CredentialExchangeTransport {
    private let client: Client

    package init(client: Client) {
        self.client = client
    }

    package func exchangeCredential(
        credentialBase64: String,
        timeout: Duration
    ) async throws -> Archebase_Auth_V1_ExchangeCredentialResponse {
        var request = Archebase_Auth_V1_ExchangeCredentialRequest()
        request.credential = credentialBase64

        var options = CallOptions.defaults
        options.timeout = timeout

        let response: ClientResponse<Archebase_Auth_V1_ExchangeCredentialResponse> = try await self.client.exchangeCredential(
            request,
            options: options,
            onResponse: { response in response }
        )

        return try response.message
    }
}

package actor CredentialAuthProvider {
    private let credentialBase64: String
    private let refreshBefore: Duration
    private let requestTimeout: Duration
    private let transport: any CredentialExchangeTransport
    private let clock: any CredentialAuthProviderClock

    private var cachedToken: AuthAccessToken?

    package init(
        credentialBase64: String,
        refreshBefore: Duration,
        requestTimeout: Duration,
        transport: any CredentialExchangeTransport,
        clock: any CredentialAuthProviderClock = SystemCredentialAuthProviderClock()
    ) {
        self.credentialBase64 = credentialBase64
        self.refreshBefore = refreshBefore
        self.requestTimeout = requestTimeout
        self.transport = transport
        self.clock = clock
    }

    package func authorizationHeader() async throws -> String {
        try await self.currentToken().authorizationHeader
    }

    package func currentToken() async throws -> AuthAccessToken {
        let now = await self.clock.now()

        if let cachedToken, !self.shouldRefresh(cachedToken, now: now) {
            return cachedToken
        }

        return try await self.refreshToken()
    }

    package func refreshToken() async throws -> AuthAccessToken {
        let credentialBase64 = self.credentialBase64.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !credentialBase64.isEmpty else {
            throw CredentialAuthProviderError.invalidCredential("credential_base64 must not be empty")
        }

        do {
            let response = try await self.transport.exchangeCredential(
                credentialBase64: credentialBase64,
                timeout: self.requestTimeout
            )
            let token = try self.makeToken(from: response)
            self.cachedToken = token
            return token
        } catch {
            self.cachedToken = nil
            throw Self.mapError(error)
        }
    }

    package func invalidateTokenCache() {
        self.cachedToken = nil
    }

    private func shouldRefresh(_ token: AuthAccessToken, now: Date) -> Bool {
        token.expiresAt.timeIntervalSince(now) <= self.refreshBefore.timeInterval
    }

    private func makeToken(
        from response: Archebase_Auth_V1_ExchangeCredentialResponse
    ) throws -> AuthAccessToken {
        guard response.tokenType.caseInsensitiveCompare("Bearer") == .orderedSame else {
            throw CredentialAuthProviderError.invalidResponse(
                "unexpected token type: \(response.tokenType)"
            )
        }

        let accessToken = response.accessToken.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !accessToken.isEmpty else {
            throw CredentialAuthProviderError.invalidResponse("exchange response returned empty access token")
        }

        guard response.expiresAtUnix > 0 else {
            throw CredentialAuthProviderError.invalidResponse("exchange response returned invalid expiration")
        }

        return AuthAccessToken(
            accessToken: accessToken,
            expiresAt: Date(timeIntervalSince1970: TimeInterval(response.expiresAtUnix))
        )
    }

    private static func mapError(_ error: any Error) -> CredentialAuthProviderError {
        if let providerError = error as? CredentialAuthProviderError {
            return providerError
        }

        if let rpcError = error as? RPCError {
            if let detail = self.decodeErrorDetail(from: rpcError) {
                let detailCode = detail.code.nilIfBlank
                let detailMessage = detail.message.nilIfBlank ?? rpcError.message
                return .authenticationFailed(code: detailCode, message: detailMessage)
            }

            return .transportFailure(code: rpcError.code, message: rpcError.message)
        }

        return .transportFailure(code: nil, message: String(describing: error))
    }

    private static func decodeErrorDetail(from rpcError: RPCError) -> Archebase_Common_V1_ErrorDetail? {
        guard let bytes = rpcError.metadata[binaryValues: authStatusDetailsMetadataKey].first(where: { _ in true }) else {
            return nil
        }

        return try? Archebase_Common_V1_ErrorDetail(serializedBytes: bytes)
    }
}

extension CredentialAuthProvider: AuthorizationHeaderProvider {
    package func invalidateAuthorization() async {
        self.invalidateTokenCache()
    }
}

private extension Duration {
    var timeInterval: TimeInterval {
        let components = self.components
        return Double(components.seconds) + Double(components.attoseconds) / 1_000_000_000_000_000_000
    }
}

private extension String {
    var nilIfBlank: String? {
        let trimmed = self.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}
