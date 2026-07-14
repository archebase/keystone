// swift-tools-version: 6.1
import PackageDescription

let package = Package(
    name: "SwiftDataGatewayClient",
    platforms: [
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
        .package(url: "https://github.com/aliyun/alibabacloud-oss-swift-sdk-v2.git", from: "0.1.1"),
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
            ]
        ),
        .target(
            name: "DGWAuth",
            dependencies: [
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
            ]
        ),
        .target(
            name: "DGWControlPlane",
            dependencies: [
                "DGWProto",
                "DGWAuth",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "GRPCProtobuf", package: "grpc-swift-protobuf"),
            ]
        ),
        .target(
            name: "DGWOss",
            dependencies: [
                "DGWControlPlane",
                "DGWProto",
                .product(name: "AlibabaCloudOSS", package: "alibabacloud-oss-swift-sdk-v2"),
                .product(name: "Crypto", package: "swift-crypto"),
            ]
        ),
        .target(
            name: "DGWStore",
            dependencies: ["DGWControlPlane"]
        ),
        .target(
            name: "DGWCore",
            dependencies: [
                "DGWAuth",
                "DGWControlPlane",
                "DGWOss",
                "DGWStore",
            ]
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
            ]
        ),
        .testTarget(
            name: "DGWProtoTests",
            dependencies: [
                "DGWProto",
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
        .testTarget(
            name: "DGWAuthTests",
            dependencies: [
                "DGWAuth",
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
        .testTarget(
            name: "DGWControlPlaneTests",
            dependencies: [
                "DGWControlPlane",
                "DGWProto",
                .product(name: "GRPCCore", package: "grpc-swift-2"),
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
        .testTarget(
            name: "DGWOssTests",
            dependencies: [
                "DGWOss",
                "DGWControlPlane",
                "DGWProto",
                .product(name: "AlibabaCloudOSS", package: "alibabacloud-oss-swift-sdk-v2"),
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
        .testTarget(
            name: "DGWStoreTests",
            dependencies: [
                "DGWStore",
                "DGWControlPlane",
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
        .testTarget(
            name: "DGWCoreTests",
            dependencies: [
                "DGWCore",
                .product(name: "Testing", package: "swift-testing"),
            ]
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
                .product(name: "GRPCNIOTransportHTTP2", package: "grpc-swift-nio-transport"),
                .product(name: "Testing", package: "swift-testing"),
            ]
        ),
    ]
)
