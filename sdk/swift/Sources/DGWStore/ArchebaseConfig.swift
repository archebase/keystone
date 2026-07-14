import DGWControlPlane
import Foundation

private let archebaseConfigMaxTagCount = 256
private let archebaseConfigMaxTagKeyBytes = 64
private let archebaseConfigMaxTagValueBytes = 2048

/// Device initialization configuration persisted in `archebase-config.json`.
public struct ArchebaseConfig: Codable, Sendable, Equatable {
    /// Upload credential returned by data-gateway device initialization.
    public var apiKey: String
    /// Platform-managed device tags merged into upload raw tags.
    public var tags: [String: String]

    enum CodingKeys: String, CodingKey {
        case apiKey = "api_key"
        case tags
    }

    /// Creates one validated Archebase device configuration value.
    public init(apiKey: String, tags: [String: String]) throws {
        self.apiKey = apiKey
        self.tags = tags
        try self.validate()
    }

    /// Validates local configuration content before it is trusted by the SDK.
    public func validate() throws {
        if self.apiKey.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
            throw DataGatewayClientError.invalidConfiguration("api_key must not be empty")
        }
        try Self.validateTags(self.tags, fieldName: "tags")
    }

    /// Encodes this configuration as stable, human-readable JSON bytes.
    public func prettyJSONData() throws -> Data {
        try self.validate()
        return try Self.encoder.encode(self)
    }

    /// Decodes and validates an Archebase configuration from JSON bytes.
    public static func decodeValidated(from data: Data) throws -> ArchebaseConfig {
        let config = try Self.decoder.decode(ArchebaseConfig.self, from: data)
        try config.validate()
        return config
    }

    /// Validates a tag map using the SDK raw-tag compatibility limits.
    public static func validateTags(_ tags: [String: String], fieldName: String = "tags") throws {
        if tags.count > archebaseConfigMaxTagCount {
            throw DataGatewayClientError.invalidConfiguration("\(fieldName) exceeds the allowed maximum item count of \(archebaseConfigMaxTagCount)")
        }
        for (key, value) in tags {
            if key.isEmpty {
                throw DataGatewayClientError.invalidConfiguration("\(fieldName) key must not be empty")
            }
            if key.utf8.count > archebaseConfigMaxTagKeyBytes {
                throw DataGatewayClientError.invalidConfiguration("\(fieldName) key exceeds the allowed maximum length of \(archebaseConfigMaxTagKeyBytes)")
            }
            if value.utf8.count > archebaseConfigMaxTagValueBytes {
                throw DataGatewayClientError.invalidConfiguration("\(fieldName) value exceeds the allowed maximum length of \(archebaseConfigMaxTagValueBytes)")
            }
            if key.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) {
                throw DataGatewayClientError.invalidConfiguration("\(fieldName) key contains unsupported control characters")
            }
            if value.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) {
                throw DataGatewayClientError.invalidConfiguration("\(fieldName) value contains unsupported control characters")
            }
        }
    }

    private static let encoder: JSONEncoder = {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return encoder
    }()

    private static let decoder = JSONDecoder()
}
