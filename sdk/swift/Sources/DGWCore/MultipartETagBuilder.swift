import Crypto
import Foundation

/// Stable module marker for DGWCore.
public enum DGWCoreModule {
    public static let name = "DGWCore"
}

package enum MultipartETagBuilderError: Error, Sendable, Equatable {
    case emptyParts
}

package enum MultipartETagBuilder {
    package static func build(partData: [Data]) throws -> String {
        let digests = partData.map(Self.md5DigestHex)
        return try self.build(fromUppercasePartMD5Hex: digests)
    }

    package static func build(fromUppercasePartMD5Hex partMD5Hex: [String]) throws -> String {
        guard !partMD5Hex.isEmpty else {
            throw MultipartETagBuilderError.emptyParts
        }

        let concatenated = partMD5Hex.joined()
        let objectDigest = Insecure.MD5.hash(data: Data(concatenated.utf8))
        return "\"\(Self.uppercaseHexString(from: objectDigest))-\(partMD5Hex.count)\""
    }

    package static func md5DigestHex(_ data: Data) -> String {
        let digest = Insecure.MD5.hash(data: data)
        return Self.uppercaseHexString(from: digest)
    }

    private static func uppercaseHexString<Digest: Sequence>(from digest: Digest) -> String where Digest.Element == UInt8 {
        digest.map { String(format: "%02X", $0) }.joined()
    }
}
