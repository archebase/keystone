import DGWControlPlane
import DGWStore

package enum DeviceInitRemoteMode: Sendable {
    case initDevice
    case reinitDevice
}

package enum DeviceInitConfigFetcher {
    package static func fetch(
        mode: DeviceInitRemoteMode,
        deviceID: String,
        transport: any DeviceInitTransport,
        sdkVersion: String,
        platform: String
    ) async throws -> ArchebaseConfig {
        do {
            let response = switch mode {
            case .initDevice:
                try await transport.initDevice(
                    deviceID: deviceID,
                    sdkVersion: sdkVersion,
                    platform: platform
                )
            case .reinitDevice:
                try await transport.reinitDevice(
                    deviceID: deviceID,
                    sdkVersion: sdkVersion,
                    platform: platform
                )
            }
            return try ArchebaseConfig(apiKey: response.apiKey, tags: response.tags)
        } catch let error as DataGatewayClientError {
            throw error
        } catch {
            throw ControlPlaneErrorMapper.map(error)
        }
    }
}
