import Foundation
import Testing

@testable import DataGatewayClient

private let manualAliyunDeviceInitEnabled = {
    let environment = ProcessInfo.processInfo.environment
    return environment["DGW_MANUAL_DEVICE_ID"]?.isEmpty == false
        && environment["DGW_MANUAL_INIT_ENDPOINT"]?.isEmpty == false
        && environment["DGW_MANUAL_CONFIG_URL"]?.isEmpty == false
}()

@Suite(.serialized)
struct ManualAliyunDeviceInitTests {
    @Test(
        .enabled(if: manualAliyunDeviceInitEnabled)
    ) func manualAliyunDeviceInitOnce() async throws {
        let environment = ProcessInfo.processInfo.environment
        let deviceID = try requiredEnvironment("DGW_MANUAL_DEVICE_ID", environment: environment)
        let endpoint = try requiredURL("DGW_MANUAL_INIT_ENDPOINT", environment: environment)
        let configPath = try requiredEnvironment("DGW_MANUAL_CONFIG_URL", environment: environment)
        let configURL = URL(fileURLWithPath: configPath)

        if FileManager.default.fileExists(atPath: configURL.path) {
            try FileManager.default.removeItem(at: configURL)
        }

        let tls: TLSMode = endpoint.scheme?.lowercased() == "https" ? .tls : .plaintext
        let initializer = try ArchebaseDeviceInitializer(
            config: DeviceInitClientConfig(configURL: configURL, tls: tls),
            initEndpoint: endpoint,
            sdkVersion: "manual-aliyun-device-init",
            platform: "macos-codex"
        )

        let config = try await initializer.initDevice(deviceID: deviceID)
        #expect(!config.apiKey.isEmpty)
        print("MANUAL_DEVICE_INIT_CONFIG_URL=\(configURL.standardizedFileURL.path)")
        print("MANUAL_DEVICE_INIT_TAG_KEYS=\(config.tags.keys.sorted().joined(separator: ","))")
    }
}

private func requiredEnvironment(
    _ name: String,
    environment: [String: String]
) throws -> String {
    guard let value = environment[name]?.trimmingCharacters(in: .whitespacesAndNewlines), !value.isEmpty else {
        throw ManualAliyunDeviceInitError.missingEnvironment(name)
    }
    return value
}

private func requiredURL(
    _ name: String,
    environment: [String: String]
) throws -> URL {
    let value = try requiredEnvironment(name, environment: environment)
    guard let url = URL(string: value) else {
        throw ManualAliyunDeviceInitError.invalidURL(name)
    }
    return url
}

private enum ManualAliyunDeviceInitError: Error, CustomStringConvertible {
    case missingEnvironment(String)
    case invalidURL(String)

    var description: String {
        switch self {
        case .missingEnvironment(let name):
            return "missing required environment variable: \(name)"
        case .invalidURL(let name):
            return "invalid URL in environment variable: \(name)"
        }
    }
}
