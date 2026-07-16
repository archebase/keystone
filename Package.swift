// swift-tools-version: 6.1
// SPDX-FileCopyrightText: 2026 ArcheBase
// SPDX-License-Identifier: MulanPSL-2.0

import PackageDescription

let package = Package(
    name: "SwiftDataGatewayClient",
    platforms: [
        .macOS(.v15),
        .iOS(.v18),
    ],
    products: [
        .library(name: "DGWProto", targets: ["DGWProto"]),
        .library(name: "DGWAuth", targets: ["DGWAuth"]),
        .library(name: "DGWControlPlane", targets: ["DGWControlPlane"]),
        .library(name: "DGWOss", targets: ["DGWOss"]),
        .library(name: "DGWStore", targets: ["DGWStore"]),
        .library(name: "DGWCore", targets: ["DGWCore"]),
        .library(name: "DataGatewayClient", targets: ["DataGatewayClient"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-protobuf.git", from: "1.31.0"),
        .package(url: "https://github.com/grpc/grpc-swift-2.git", from: "2.1.0"),
        .package(url: "https://github.com/grpc/grpc-swift-nio-transport.git", from: "2.1.0"),
        .package(url: "https://github.com/grpc/grpc-swift-protobuf.git", from: "2.1.0"),
        .package(url: "https://github.com/apple/swift-crypto.git", from: "3.12.0"),
        .package(url: "https://github.com/swiftlang/swift-testing.git", exact: "6.1.3"),
    ],
    targets: [
        .target(
            name: "DGWProto",
            dependencies: [
                .product(name: "SwiftProtobuf", package: "swift-protobuf"),
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ],
            path: "sdk/swift/Sources/DGWProto"
        ),
        .target(
            name: "DGWAuth",
            dependencies: [
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
            ],
            path: "sdk/swift/Sources/DGWAuth"
        ),
        .target(
            name: "DGWControlPlane",
            dependencies: [
                "DGWProto",
                "DGWAuth",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2Posix", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ],
            path: "sdk/swift/Sources/DGWControlPlane"
        ),
        .target(
            name: "DGWOss",
            dependencies: [
                "DGWControlPlane",
                "DGWProto",
                .product(name: "Crypto", package: "swift-crypto"),
            ],
            path: "sdk/swift/Sources/DGWOss"
        ),
        .target(
            name: "DGWStore",
            dependencies: ["DGWControlPlane"],
            path: "sdk/swift/Sources/DGWStore"
        ),
        .target(
            name: "DGWCore",
            dependencies: [
                "DGWAuth",
                "DGWControlPlane",
                "DGWOss",
                "DGWStore",
            ],
            path: "sdk/swift/Sources/DGWCore"
        ),
        .target(
            name: "DataGatewayClient",
            dependencies: [
                "DGWCore",
                "DGWControlPlane",
                "DGWOss",
                "DGWProto",
                "DGWStore",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Crypto", package: "swift-crypto"),
            ],
            path: "sdk/swift/Sources/DataGatewayClient"
        ),
        .testTarget(
            name: "DGWProtoTests",
            dependencies: [
                "DGWProto",
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWProtoTests"
        ),
        .testTarget(
            name: "DGWAuthTests",
            dependencies: [
                "DGWAuth",
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWAuthTests"
        ),
        .testTarget(
            name: "DGWControlPlaneTests",
            dependencies: [
                "DGWControlPlane",
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWControlPlaneTests"
        ),
        .testTarget(
            name: "DGWOssTests",
            dependencies: [
                "DGWOss",
                "DGWControlPlane",
                "DGWProto",
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWOssTests"
        ),
        .testTarget(
            name: "DGWStoreTests",
            dependencies: [
                "DGWStore",
                "DGWControlPlane",
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWStoreTests"
        ),
        .testTarget(
            name: "DGWCoreTests",
            dependencies: [
                "DGWCore",
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DGWCoreTests"
        ),
        .testTarget(
            name: "DataGatewayClientIntegrationTests",
            dependencies: [
                "DataGatewayClient",
                "DGWAuth",
                "DGWCore",
                "DGWControlPlane",
                "DGWOss",
                "DGWProto",
                "DGWStore",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2Posix", package: "grpc-swift-nio-transport"),
                .product(name: "Testing", package: "swift-testing"),
            ],
            path: "sdk/swift/Tests/DataGatewayClientIntegrationTests"
        ),
    ]
)
