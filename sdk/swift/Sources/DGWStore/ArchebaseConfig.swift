// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import DGWControlPlane
import Foundation

private let archebaseConfigMaxTagCount = 256
private let archebaseConfigMaxTagKeyBytes = 64
private let archebaseConfigMaxTagValueBytes = 2048

/// Device initialization configuration persisted in `archebase-config.json`.
public struct ArchebaseConfig: Codable, Sendable, Equatable {
    /// Upload credential returned by data-gateway device initialization.
    public var apiKey: String = ""
    /// Platform-managed device tags merged into upload raw tags.
    public var tags: [String: String]

    enum CodingKeys: String, CodingKey {
        case credentialStore = "credential_store"
        case tags
    }

    private static let keychainCredentialStore = "keychain"

    /// Creates one validated Archebase device configuration value.
    public init(apiKey: String, tags: [String: String]) throws {
        self.apiKey = apiKey
        self.tags = tags
        try self.validate()
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let credentialStore = try container.decode(String.self, forKey: .credentialStore)
        guard credentialStore == Self.keychainCredentialStore else {
            throw DataGatewayClientError.invalidConfiguration("credential_store must be keychain")
        }
        self.tags = try container.decode([String: String].self, forKey: .tags)
        try Self.validateTags(self.tags, fieldName: "tags")
    }

    public func encode(to encoder: any Encoder) throws {
        try self.validate()
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode(Self.keychainCredentialStore, forKey: .credentialStore)
        try container.encode(self.tags, forKey: .tags)
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
    package static func decodePersisted(from data: Data) throws -> ArchebaseConfig {
        let config = try Self.decoder.decode(ArchebaseConfig.self, from: data)
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
