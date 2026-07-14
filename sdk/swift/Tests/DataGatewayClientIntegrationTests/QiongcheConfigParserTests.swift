import Foundation
import Testing

@testable import DataGatewayClient

@Test func qiongcheConfigParserParsesValidConfig() throws {
    let parsed = try QiongcheConfigParser.parse(validQiongcheConfig(deviceID: " robot-001 "))

    #expect(parsed.deviceID == "robot-001")
    #expect(parsed.resolvedEndpoints.auth == URL(string: "http://auth.example.com:50051")!)
    #expect(parsed.resolvedEndpoints.gateway == URL(string: "http://gateway.example.com:50053")!)
    #expect(parsed.resolvedEndpoints.deviceInit == URL(string: "https://init.example.com:443")!)
    #expect(!parsed.normalizedEndpointsJSONString.contains("device_id"))
    #expect(throws: Never.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(parsed.normalizedEndpointsJSONData)
    }
}

@Test func qiongcheEndpointFingerprintIgnoresJSONFieldOrder() throws {
    let reordered = """
    {
      "deviceInit": { "port": 443, "host": "init.example.com", "scheme": "https" },
      "gateway": { "host": "gateway.example.com", "scheme": "http", "port": 50053 },
      "auth": { "port": 50051, "scheme": "http", "host": "auth.example.com" },
      "device_id": "robot-001"
    }
    """

    let parsed = try QiongcheConfigParser.parse(validQiongcheConfig())
    let reorderedParsed = try QiongcheConfigParser.parse(reordered)

    #expect(parsed.normalizedEndpointsJSONData == reorderedParsed.normalizedEndpointsJSONData)
    #expect(parsed.endpointsSHA256Hex == reorderedParsed.endpointsSHA256Hex)
    #expect(parsed.endpointsSHA256Hex.count == 64)
}

@Test func qiongcheEndpointFingerprintChangesWhenEndpointChanges() throws {
    let parsed = try QiongcheConfigParser.parse(validQiongcheConfig())
    let changed = try QiongcheConfigParser.parse(validQiongcheConfig(authHost: "auth-alt.example.com"))

    #expect(parsed.endpointsSHA256Hex != changed.endpointsSHA256Hex)
    #expect(throws: Never.self) {
        try ArchebasePublicEndpoints.decodeEndpoints(changed.normalizedEndpointsJSONData)
    }
}

@Test func qiongcheConfigParserRejectsMissingDeviceID() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(error == .invalidConfigString("device_id is required"))
}

@Test func qiongcheConfigParserInvalidConfigUsesQiongcheSDKErrorWithoutEchoingConfig() {
    let configString = """
    {
      "device_id": "robot-001",
      "auth": { "scheme": "grpc", "host": "auth.example.com", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """

    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse(configString)
    }

    if case .invalidConfigString(let message) = error {
        #expect(message == "auth.scheme must be http or https")
        #expect(!message.contains("robot-001"))
        #expect(!message.contains("auth.example.com"))
    } else {
        Issue.record("expected invalidConfigString")
    }
}

@Test func qiongcheConfigParserRejectsEmptyDeviceID() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse(validQiongcheConfig(deviceID: "   "))
    }

    #expect(error == .invalidConfigString("device_id must not be empty"))
}

@Test func qiongcheConfigParserRejectsControlCharacterDeviceID() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot\\u0007001",
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(error == .invalidConfigString("device_id contains unsupported control characters"))
}

@Test func qiongcheConfigParserRejectsMissingEndpoint() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(error == .invalidConfigString("auth endpoint is required"))
}

@Test func qiongcheConfigParserRejectsMissingGatewayAndDeviceInit() {
    #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 }
        }
        """)
    }
}

@Test func qiongcheConfigParserRejectsWrongDeviceIDType() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": 1001,
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(error == .invalidConfigString("device_id is required"))
}

@Test func qiongcheConfigParserRejectsInvalidEndpointSchemeHostAndPort() {
    #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "grpc", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "http", "host": "   ", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 65536 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }
}

@Test func qiongcheConfigParserRejectsUnsupportedTopLevelField() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "scheme": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 },
          "extra": true
        }
        """)
    }

    #expect(error == .invalidConfigString("unsupported top-level field 'extra'"))
}

@Test func qiongcheConfigParserReusesEndpointValidation() {
    let error = #expect(throws: QiongcheSDKError.self) {
        try QiongcheConfigParser.parse("""
        {
          "device_id": "robot-001",
          "auth": { "schema": "http", "host": "auth.example.com", "port": 50051 },
          "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
          "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
        }
        """)
    }

    #expect(error == .invalidConfigString("auth.schema is not supported; use scheme"))
}

func validQiongcheConfig(deviceID: String = "robot-001", authHost: String = "auth.example.com") -> String {
    """
    {
      "device_id": "\(deviceID)",
      "auth": { "scheme": "http", "host": "\(authHost)", "port": 50051 },
      "gateway": { "scheme": "http", "host": "gateway.example.com", "port": 50053 },
      "deviceInit": { "scheme": "https", "host": "init.example.com", "port": 443 }
    }
    """
}
