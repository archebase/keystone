import DGWControlPlane
import Foundation
import Testing

@testable import DGWStore

@Test func archebaseConfigDecodesPlanExampleJSON() throws {
    let json = Data(#"{"api_key":"credential-v1","tags":{"key1":"value1","key2":"value2"}}"#.utf8)

    let config = try ArchebaseConfig.decodeValidated(from: json)

    #expect(config.apiKey == "credential-v1")
    #expect(config.tags == ["key1": "value1", "key2": "value2"])
}

@Test func archebaseConfigEncodesSnakeCaseAPIKey() throws {
    let config = try ArchebaseConfig(apiKey: "credential-v1", tags: ["tag": "value"])

    let json = String(data: try config.prettyJSONData(), encoding: .utf8) ?? ""

    #expect(json.contains("\"api_key\""))
    #expect(!json.contains("apiKey"))
}

@Test func archebaseConfigRejectsEmptyAPIKey() {
    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try ArchebaseConfig(apiKey: "  ", tags: [:])
    }

    #expect(error == .invalidConfiguration("api_key must not be empty"))
}

@Test func archebaseConfigRejectsMissingTags() {
    let json = Data(#"{"api_key":"credential-v1"}"#.utf8)

    #expect(throws: DecodingError.self) {
        _ = try ArchebaseConfig.decodeValidated(from: json)
    }
}

@Test func archebaseConfigRejectsEmptyTagKey() {
    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try ArchebaseConfig(apiKey: "credential-v1", tags: ["": "value"])
    }

    #expect(error == .invalidConfiguration("tags key must not be empty"))
}

@Test func archebaseConfigRejectsTooLongTagValue() {
    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try ArchebaseConfig(apiKey: "credential-v1", tags: ["tag": String(repeating: "x", count: 2049)])
    }

    #expect(error == .invalidConfiguration("tags value exceeds the allowed maximum length of 2048"))
}

@Test func archebaseConfigRejectsControlCharacters() {
    let error = #expect(throws: DataGatewayClientError.self) {
        _ = try ArchebaseConfig(apiKey: "credential-v1", tags: ["ta\u{0001}g": "value"])
    }

    #expect(error == .invalidConfiguration("tags key contains unsupported control characters"))
}
