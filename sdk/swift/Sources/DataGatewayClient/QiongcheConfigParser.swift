import Crypto
import DGWControlPlane
import Foundation

public enum QiongcheSDKError: Error, Sendable, Equatable {
    case invalidConfigString(String)
}

package struct QiongcheBootstrapConfig: Sendable, Equatable {
    package let deviceID: String
    package let normalizedEndpointsJSONData: Data
    package let resolvedEndpoints: ArchebasePublicEndpoints.Resolved

    package var normalizedEndpointsJSONString: String {
        String(decoding: self.normalizedEndpointsJSONData, as: UTF8.self)
    }

    package var endpointsSHA256Hex: String {
        QiongcheConfigParser.sha256Hex(self.normalizedEndpointsJSONData)
    }
}

package enum QiongcheConfigParser {
    private static let allowedTopLevelFields: Set<String> = ["auth", "gateway", "deviceInit", "device_id"]

    package static func parse(_ configString: String) throws -> QiongcheBootstrapConfig {
        guard !configString.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw QiongcheSDKError.invalidConfigString("configString must not be empty")
        }

        let data = Data(configString.utf8)
        let topLevel: [String: Any]
        do {
            guard let parsed = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                throw QiongcheSDKError.invalidConfigString("configString must be a JSON object")
            }
            topLevel = parsed
        } catch let error as QiongcheSDKError {
            throw error
        } catch {
            throw QiongcheSDKError.invalidConfigString("configString is not valid JSON")
        }

        let unsupportedFields = Set(topLevel.keys).subtracting(Self.allowedTopLevelFields).sorted()
        if let unsupportedField = unsupportedFields.first {
            throw QiongcheSDKError.invalidConfigString("unsupported top-level field '\(unsupportedField)'")
        }

        let deviceID = try Self.parseDeviceID(topLevel["device_id"])
        let endpointsData = try Self.endpointsData(from: topLevel)

        do {
            let normalizedData = try ArchebasePublicEndpoints.normalizedJSONData(from: endpointsData)
            let resolved = try ArchebasePublicEndpoints.decodeEndpoints(normalizedData)
            return QiongcheBootstrapConfig(
                deviceID: deviceID,
                normalizedEndpointsJSONData: normalizedData,
                resolvedEndpoints: resolved
            )
        } catch let error as DataGatewayClientError {
            throw QiongcheSDKError.invalidConfigString(Self.endpointMessage(from: error))
        } catch {
            throw QiongcheSDKError.invalidConfigString("endpoint configuration is invalid")
        }
    }

    private static func parseDeviceID(_ value: Any?) throws -> String {
        guard let rawDeviceID = value as? String else {
            throw QiongcheSDKError.invalidConfigString("device_id is required")
        }

        let deviceID = rawDeviceID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !deviceID.isEmpty else {
            throw QiongcheSDKError.invalidConfigString("device_id must not be empty")
        }

        guard !deviceID.unicodeScalars.contains(where: { $0.properties.generalCategory == .control }) else {
            throw QiongcheSDKError.invalidConfigString("device_id contains unsupported control characters")
        }

        return deviceID
    }

    private static func endpointsData(from topLevel: [String: Any]) throws -> Data {
        for field in ["auth", "gateway", "deviceInit"] where topLevel[field] == nil {
            throw QiongcheSDKError.invalidConfigString("\(field) endpoint is required")
        }

        let endpointsObject: [String: Any] = [
            "auth": topLevel["auth"] as Any,
            "gateway": topLevel["gateway"] as Any,
            "deviceInit": topLevel["deviceInit"] as Any,
        ]

        do {
            return try JSONSerialization.data(withJSONObject: endpointsObject, options: [.sortedKeys])
        } catch {
            throw QiongcheSDKError.invalidConfigString("endpoint configuration is invalid")
        }
    }

    private static func endpointMessage(from error: DataGatewayClientError) -> String {
        switch error {
        case .invalidConfiguration(let message), .persistenceFailed(let message):
            return message
        case .endpointsAlreadyInitialized, .endpointsNotInitialized:
            return "endpoint configuration is invalid"
        default:
            return "endpoint configuration is invalid"
        }
    }

    package static func sha256Hex(_ data: Data) -> String {
        SHA256.hash(data: data)
            .map { String(format: "%02x", $0) }
            .joined()
    }
}
