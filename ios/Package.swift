// swift-tools-version: 6.2

import PackageDescription

let package = Package(
    name: "ConseilCore",
    platforms: [
        .iOS(.v17),
        .macOS(.v14),
    ],
    products: [
        .library(name: "ConseilCore", targets: ["ConseilCore"]),
    ],
    targets: [
        .target(
            name: "ConseilCore",
            path: "Conseil",
            exclude: [
                "AppModel.swift",
                "Assets.xcassets",
                "ConseilApp.swift",
                "ConversationListView.swift",
                "ConversationView.swift",
                "RootView.swift",
                "SettingsView.swift",
                "TraceView.swift",
            ],
            sources: ["APIClient.swift", "KeychainStore.swift", "Models.swift"],
            linkerSettings: [.linkedFramework("Security")]
        ),
        .testTarget(
            name: "ConseilCoreTests",
            dependencies: ["ConseilCore"],
            path: "ConseilCoreTests"
        ),
    ]
)
