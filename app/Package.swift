// swift-tools-version: 6.2
import PackageDescription

let strict: [SwiftSetting] = [
    .swiftLanguageMode(.v6),
]

let package = Package(
    name: "Fevom",
    platforms: [.macOS(.v26)],
    products: [
        .executable(name: "Fevom", targets: ["Fevom"]),
        .library(name: "FevomAPI", targets: ["FevomAPI"]),
        .library(name: "FevomStores", targets: ["FevomStores"]),
    ],
    dependencies: [
        .package(url: "https://github.com/migueldeicaza/SwiftTerm", exact: "1.20.0"),
        .package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.10.0"),
    ],
    targets: [
        .target(name: "FevomAPI", swiftSettings: strict),
        .target(name: "FevomDocker", swiftSettings: strict),
        .target(name: "FevomStores", dependencies: ["FevomAPI", "FevomDocker"], swiftSettings: strict),
        .executableTarget(name: "Fevom", dependencies: ["FevomAPI", "FevomDocker", "FevomStores", .product(name: "SwiftTerm", package: "SwiftTerm"),
                                                        .product(name: "Sparkle", package: "Sparkle")],
                          swiftSettings: strict,
                          linkerSettings: [.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"])]),
        .testTarget(name: "FevomAPITests", dependencies: ["FevomAPI"], swiftSettings: strict),
        .testTarget(name: "FevomStoresTests", dependencies: ["FevomStores", "FevomAPI", "FevomDocker"], swiftSettings: strict),
    ]
)
