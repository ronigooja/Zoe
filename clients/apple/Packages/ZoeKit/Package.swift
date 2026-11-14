// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "ZoeKit",
    defaultLocalization: "en",
    platforms: [
        .macOS(.v14),
        .iOS(.v17)
    ],
    products: [
        .library(name: "ZoeKit", targets: ["ZoeKit"]),
        .library(name: "ZoeUI", targets: ["ZoeUI"])
    ],
    dependencies: [
        .package(url: "https://github.com/stasel/WebRTC.git", revision: "44f36788497f701bc8c0306de3a1c2511d56019d"),
        .package(url: "https://github.com/BBC6BAE9/a2ui-swift.git", revision: "16476ba2cb3fcb4c4bcb141dfa4c2804adbdedb0")
    ],
    targets: [
        .target(name: "ZoeKit", dependencies: [.product(name: "A2UISwiftCore", package: "a2ui-swift")]),
        .target(name: "ZoeUI", dependencies: ["ZoeKit", .product(name: "WebRTC", package: "WebRTC"), .product(name: "A2UISwiftUI", package: "a2ui-swift")], resources: [.process("Resources")]),
        .testTarget(name: "ZoeKitTests", dependencies: ["ZoeKit", "ZoeUI"])
    ]
)
