import Foundation
import Testing

import DGWAuth
import DGWProto
import GRPCCore

private let authStatusDetailsMetadataKey = "grpc-status-details-bin"

private struct Invocation: Equatable, Sendable {
    let credentialBase64: String
    let timeout: Duration
}

private actor TestClock: CredentialAuthProviderClock {
    private var current: Date

    init(now: Date) {
        self.current = now
    }

    func now() async -> Date {
        self.current
    }

    func advance(seconds: TimeInterval) {
        self.current = self.current.addingTimeInterval(seconds)
    }
}

private actor MockCredentialExchangeTransport: CredentialExchangeTransport {
    enum Outcome: Sendable {
        case success(Archebase_Auth_V1_ExchangeCredentialResponse)
        case failure(Failure)
    }

    enum Failure: Error, Sendable, Equatable {
        case rpc(RPCError)
        case message(String)
    }

    private var outcomes: [Outcome]
    private var recordedInvocations: [Invocation] = []

    init(outcomes: [Outcome]) {
        self.outcomes = outcomes
    }

    func exchangeCredential(
        credentialBase64: String,
        timeout: Duration
    ) async throws -> Archebase_Auth_V1_ExchangeCredentialResponse {
        self.recordedInvocations.append(
            Invocation(credentialBase64: credentialBase64, timeout: timeout)
        )

        guard !self.outcomes.isEmpty else {
            throw Failure.message("missing mock outcome")
        }

        let outcome = self.outcomes.removeFirst()
        switch outcome {
        case .success(let response):
            return response
        case .failure(let failure):
            switch failure {
            case .rpc(let rpcError):
                throw rpcError
            case .message(let message):
                throw Failure.message(message)
            }
        }
    }

    func invocations() -> [Invocation] {
        self.recordedInvocations
    }
}

@Test func emptyCredentialFailsBeforeTransportCall() async {
    let clock = TestClock(now: Date(timeIntervalSince1970: 1_000))
    let transport = MockCredentialExchangeTransport(
        outcomes: [.success(makeResponse(accessToken: "token-1", expiresAtUnix: 2_000))]
    )
    let provider = CredentialAuthProvider(
        credentialBase64: "   ",
        refreshBefore: .seconds(30),
        requestTimeout: .seconds(5),
        transport: transport,
        clock: clock
    )

    let error = await #expect(throws: CredentialAuthProviderError.self) {
        try await provider.authorizationHeader()
    }

    #expect(error == .invalidCredential("credential_base64 must not be empty"))
    #expect(await transport.invocations().isEmpty)
}

@Test func cachedTokenSkipsSecondExchange() async throws {
    let clock = TestClock(now: Date(timeIntervalSince1970: 1_000))
    let transport = MockCredentialExchangeTransport(
        outcomes: [.success(makeResponse(accessToken: "token-1", expiresAtUnix: 2_000))]
    )
    let provider = CredentialAuthProvider(
        credentialBase64: "credential-base64",
        refreshBefore: .seconds(60),
        requestTimeout: .seconds(5),
        transport: transport,
        clock: clock
    )

    let firstHeader = try await provider.authorizationHeader()
    let secondHeader = try await provider.authorizationHeader()

    #expect(firstHeader == "Bearer token-1")
    #expect(secondHeader == "Bearer token-1")
    #expect(await transport.invocations().count == 1)
}

@Test func tokenRefreshesInsideRefreshWindow() async throws {
    let clock = TestClock(now: Date(timeIntervalSince1970: 1_000))
    let transport = MockCredentialExchangeTransport(
        outcomes: [
            .success(makeResponse(accessToken: "token-1", expiresAtUnix: 1_100)),
            .success(makeResponse(accessToken: "token-2", expiresAtUnix: 1_300)),
        ]
    )
    let provider = CredentialAuthProvider(
        credentialBase64: "credential-base64",
        refreshBefore: .seconds(30),
        requestTimeout: .seconds(5),
        transport: transport,
        clock: clock
    )

    let firstHeader = try await provider.authorizationHeader()
    await clock.advance(seconds: 80)
    let secondHeader = try await provider.authorizationHeader()

    #expect(firstHeader == "Bearer token-1")
    #expect(secondHeader == "Bearer token-2")
    #expect(await transport.invocations().count == 2)
}

@Test func repeatedExchangeFailuresRemainVisible() async {
    let clock = TestClock(now: Date(timeIntervalSince1970: 1_000))
    let failure = makeRPCError(
        code: .unauthenticated,
        message: "invalid credential",
        detailCode: "AUTH_INVALID_CREDENTIAL",
        detailMessage: "invalid credential"
    )
    let transport = MockCredentialExchangeTransport(
        outcomes: [
            .failure(.rpc(failure)),
            .failure(.rpc(failure)),
        ]
    )
    let provider = CredentialAuthProvider(
        credentialBase64: "credential-base64",
        refreshBefore: .seconds(30),
        requestTimeout: .seconds(5),
        transport: transport,
        clock: clock
    )

    let firstError = await #expect(throws: CredentialAuthProviderError.self) {
        try await provider.authorizationHeader()
    }
    let secondError = await #expect(throws: CredentialAuthProviderError.self) {
        try await provider.authorizationHeader()
    }

    let expected = CredentialAuthProviderError.authenticationFailed(
        code: "AUTH_INVALID_CREDENTIAL",
        message: "invalid credential"
    )
    #expect(firstError == expected)
    #expect(secondError == expected)
    #expect(await transport.invocations().count == 2)
}

@Test func authorizationHeaderUsesBearerPrefixExactly() async throws {
    let clock = TestClock(now: Date(timeIntervalSince1970: 1_000))
    let transport = MockCredentialExchangeTransport(
        outcomes: [.success(makeResponse(accessToken: "token-exact", expiresAtUnix: 2_000))]
    )
    let provider = CredentialAuthProvider(
        credentialBase64: "credential-base64",
        refreshBefore: .seconds(60),
        requestTimeout: .seconds(5),
        transport: transport,
        clock: clock
    )

    let header = try await provider.authorizationHeader()

    #expect(header == "Bearer token-exact")
}

private func makeResponse(
    accessToken: String,
    expiresAtUnix: Int64,
    tokenType: String = "Bearer"
) -> Archebase_Auth_V1_ExchangeCredentialResponse {
    var response = Archebase_Auth_V1_ExchangeCredentialResponse()
    response.accessToken = accessToken
    response.expiresAtUnix = expiresAtUnix
    response.tokenType = tokenType
    return response
}

private func makeRPCError(
    code: RPCError.Code,
    message: String,
    detailCode: String,
    detailMessage: String
) -> RPCError {
    var detail = Archebase_Common_V1_ErrorDetail()
    detail.code = detailCode
    detail.message = detailMessage

    let bytes: [UInt8]
    do {
        bytes = try detail.serializedBytes()
    } catch {
        Issue.record("failed to encode error detail: \(error)")
        return RPCError(code: code, message: message)
    }

    var metadata = Metadata()
    metadata.addBinary(bytes, forKey: authStatusDetailsMetadataKey)
    return RPCError(code: code, message: message, metadata: metadata)
}
