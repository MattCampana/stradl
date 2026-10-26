// swift-tools-version:5.10
// The Stradl app: a native window onto the sync agent (status, the Circle,
// sessions, project folders, settings and logs). Built into Stradl.app by
// scripts/build-app.sh; it talks to the agent's local dashboard API and to
// the stradl command, never to storage itself.
import PackageDescription

let package = Package(
    name: "Stradl",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "Stradl",
            path: "Sources/Stradl"
        ),
    ]
)
