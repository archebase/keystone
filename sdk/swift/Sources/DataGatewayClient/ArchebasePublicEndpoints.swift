import DGWControlPlane
import DGWStore
import Foundation

/// Runtime store for Archebase public service endpoints.
public enum ArchebasePublicEndpoints {
    package struct Resolved: Sendable, Equatable {
        package var auth: URL
        package var gateway: URL
        package var deviceInit: URL
        package var authTLS: TLSMode
        package var gatewayTLS: TLSMode
        package var deviceInitTLS: TLSMode
    }

    public static let endpointsFileName = "archebase-endpoints.json"

    public static func defaultEndpointsURL() throws -> URL {
        guard let applicationSupport = FileManager.default.urls(
            for: .applicationSupportDirectory,
            in: .userDomainMask
        ).first else {
            throw DataGatewayClientError.invalidConfiguration("application support directory is unavailable")
        }

        return applicationSupport
            .appendingPathComponent("Archebase", isDirectory: true)
            .appendingPathComponent(Self.endpointsFileName, isDirectory: false)
            .standardizedFileURL
    }

    package static func decodeEndpoints(_ data: Data) throws -> Resolved {
        do {
            let payload = try JSONDecoder().decode(EndpointsPayload.self, from: data)
            let auth = try payload.auth.resolvedURL(fieldName: "auth")
            let gateway = try payload.gateway.resolvedURL(fieldName: "gateway")
            let deviceInit = try payload.deviceInit.resolvedURL(fieldName: "deviceInit")
            return Resolved(
                auth: auth.url,
                gateway: gateway.url,
                deviceInit: deviceInit.url,
                authTLS: auth.tls,
                gatewayTLS: gateway.tls,
                deviceInitTLS: deviceInit.tls
            )
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.invalidConfiguration(
                "failed to decode archebase endpoints: \(error.localizedDescription)"
            )
        }
    }

    package static func normalizedJSONData(endpointsJSON: String) throws -> Data {
        guard !endpointsJSON.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("archebase endpoints json must not be empty")
        }

        return try self.normalizedJSONData(from: Data(endpointsJSON.utf8))
    }

    package static func normalizedJSONData(from data: Data) throws -> Data {
        let resolved = try self.decodeEndpoints(data)
        return try self.normalizedJSONData(from: resolved)
    }

    package static func normalizedJSONString(endpointsJSON: String) throws -> String {
        let data = try self.normalizedJSONData(endpointsJSON: endpointsJSON)
        guard let string = String(data: data, encoding: .utf8) else {
            throw DataGatewayClientError.persistenceFailed("failed to encode normalized archebase endpoints")
        }
        return string
    }

    package static func load(endpointsURL: URL) throws -> Resolved {
        let resolvedURL = endpointsURL.standardizedFileURL
        guard FileManager.default.fileExists(atPath: resolvedURL.path) else {
            throw DataGatewayClientError.endpointsNotInitialized(endpointsURL: resolvedURL)
        }

        do {
            let data = try Data(contentsOf: resolvedURL)
            return try Self.decodeEndpoints(data)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.invalidConfiguration(
                "failed to load archebase endpoints: \(error.localizedDescription)"
            )
        }
    }

    public static func initialize(endpointsJSON: String, endpointsURL: URL) throws {
        guard !endpointsJSON.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("archebase endpoints json must not be empty")
        }

        let data = Data(endpointsJSON.utf8)
        let expected = try Self.decodeEndpoints(data)
        let resolvedURL = endpointsURL.standardizedFileURL
        let fileManager = FileManager.default

        if fileManager.fileExists(atPath: resolvedURL.path) {
            let existing = try Self.load(endpointsURL: resolvedURL)
            guard existing == expected else {
                throw DataGatewayClientError.endpointsAlreadyInitialized(endpointsURL: resolvedURL)
            }
            return
        }

        try Self.atomicWrite(data, expected: expected, to: resolvedURL, fileManager: fileManager)
    }

    package static func replace(endpointsJSON: String, endpointsURL: URL) throws {
        let data = try Self.normalizedJSONData(endpointsJSON: endpointsJSON)
        let expected = try Self.decodeEndpoints(data)
        let resolvedURL = endpointsURL.standardizedFileURL
        try Self.atomicWrite(data, expected: expected, to: resolvedURL, fileManager: .default, replacingExisting: true)
    }

    private static func normalizedJSONData(from resolved: Resolved) throws -> Data {
        try Self.normalizedEncoder.encode(NormalizedEndpointsPayload(
            auth: NormalizedEndpointPayload(url: resolved.auth),
            gateway: NormalizedEndpointPayload(url: resolved.gateway),
            deviceInit: NormalizedEndpointPayload(url: resolved.deviceInit)
        ))
    }

    private static func atomicWrite(
        _ data: Data,
        expected: Resolved,
        to endpointsURL: URL,
        fileManager: FileManager,
        replacingExisting: Bool = false
    ) throws {
        var equivalentExistingFileWonRace = false
        do {
            try AtomicFileWriter.write(data, to: endpointsURL, fileManager: fileManager) { temporaryURL, destination, fileManager in
                if replacingExisting {
                    try AtomicFileWriter.replaceOrMoveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
                } else {
                    do {
                        try AtomicFileWriter.moveTemporaryItem(temporaryURL, to: destination, fileManager: fileManager)
                    } catch {
                        if fileManager.fileExists(atPath: destination.path) {
                            let existing = try Self.loadPersistedEndpoints(endpointsURL: destination)
                            guard existing == expected else {
                                throw DataGatewayClientError.endpointsAlreadyInitialized(endpointsURL: destination)
                            }
                            equivalentExistingFileWonRace = true
                            return
                        }
                        throw error
                    }
                }
            }

            if equivalentExistingFileWonRace {
                return
            }

            let loaded = try Self.loadPersistedEndpoints(endpointsURL: endpointsURL)
            guard loaded == expected else {
                throw DataGatewayClientError.persistenceFailed("archebase endpoints verification failed after write")
            }
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.persistenceFailed(
                "failed to write archebase endpoints: \(error.localizedDescription)"
            )
        }
    }

    private static func loadPersistedEndpoints(endpointsURL: URL) throws -> Resolved {
        do {
            let data = try Data(contentsOf: endpointsURL)
            return try Self.decodeEndpoints(data)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw DataGatewayClientError.persistenceFailed(
                "failed to verify persisted archebase endpoints at \(endpointsURL.path): \(error.localizedDescription)"
            )
        }
    }

    private static let normalizedEncoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        return encoder
    }()
}

private struct EndpointsPayload: Decodable {
    var auth: EndpointPayload
    var gateway: EndpointPayload
    var deviceInit: EndpointPayload
}

private struct NormalizedEndpointsPayload: Encodable {
    var auth: NormalizedEndpointPayload
    var gateway: NormalizedEndpointPayload
    var deviceInit: NormalizedEndpointPayload
}

private struct NormalizedEndpointPayload: Encodable {
    var scheme: String
    var host: String
    var port: Int

    init(url: URL) throws {
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let scheme = components.scheme,
              let host = components.host,
              let port = components.port else {
            throw DataGatewayClientError.invalidConfiguration("normalized endpoint is not a valid URL")
        }
        self.scheme = scheme
        self.host = host
        self.port = port
    }
}

private struct EndpointPayload: Decodable {
    private static let allowedFields: Set<String> = ["scheme", "host", "port"]

    var scheme: String?
    var host: String?
    var port: Int?
    var unsupportedFields: [String]
    var hasLegacySchemaField: Bool

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: DynamicCodingKey.self)
        let keys = container.allKeys.map(\.stringValue)
        self.unsupportedFields = keys.filter { !Self.allowedFields.contains($0) }.sorted()
        self.hasLegacySchemaField = keys.contains("schema")
        self.scheme = try container.decodeIfPresent(String.self, forKey: DynamicCodingKey("scheme"))
        self.host = try container.decodeIfPresent(String.self, forKey: DynamicCodingKey("host"))
        self.port = try container.decodeIfPresent(Int.self, forKey: DynamicCodingKey("port"))
    }

    func resolvedURL(fieldName: String) throws -> (url: URL, tls: TLSMode) {
        if self.hasLegacySchemaField {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).schema is not supported; use scheme")
        }

        if let unsupportedField = self.unsupportedFields.first {
            throw DataGatewayClientError.invalidConfiguration(
                "\(fieldName) contains unsupported field '\(unsupportedField)'"
            )
        }

        guard let scheme else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).scheme is required")
        }
        let normalizedScheme = scheme.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        let tls: TLSMode
        switch normalizedScheme {
        case "http":
            tls = .plaintext
        case "https":
            tls = .tls
        default:
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).scheme must be http or https")
        }

        guard let host else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).host is required")
        }
        let normalizedHost = host.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !normalizedHost.isEmpty else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).host must not be empty")
        }

        guard let port else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).port is required")
        }
        guard (1 ... 65535).contains(port) else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName).port must be between 1 and 65535")
        }

        var components = URLComponents()
        components.scheme = normalizedScheme
        components.host = normalizedHost
        components.port = port
        guard let url = components.url else {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName) endpoint is not a valid URL")
        }

        return (url, tls)
    }
}

private struct DynamicCodingKey: CodingKey {
    var stringValue: String
    var intValue: Int?

    init(_ stringValue: String) {
        self.stringValue = stringValue
        self.intValue = nil
    }

    init?(stringValue: String) {
        self.init(stringValue)
    }

    init?(intValue: Int) {
        self.stringValue = "\(intValue)"
        self.intValue = intValue
    }
}
