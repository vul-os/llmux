// swift-tools-version:5.9
//
// No `unsafeFlags`, no module map, no system-library target, and no bridging
// header. The C ABI is reached with `dlopen`/`dlsym` plus `@convention(c)`
// function types, which means:
//
//   - `swift build` works with nothing on the machine but a Swift toolchain.
//   - The library is located at RUN time, so one build works whether
//     `libllmux` sits in `dist/ffi/`, on `DYLD_LIBRARY_PATH`, or at whatever
//     `$LLMUX_LIBRARY` names.
//   - This package can be a dependency of another package. A target with
//     `unsafeFlags` cannot be, which rules out the link-time approach for
//     anything published.
//
// Zero external dependencies.

import PackageDescription

let package = Package(
    name: "LLMux",
    platforms: [
        // Tested on macOS 15.7.3 (Apple silicon) with Swift 6.1.2. The floor is
        // 13 for `URLSession.bytes(for:)`, which the sidecar's SSE stream uses.
        .macOS(.v13)
    ],
    products: [
        .library(name: "LLMux", targets: ["LLMux"]),
        .executable(name: "llmux-direct-example", targets: ["llmux-direct-example"]),
        .executable(name: "llmux-sidecar-example", targets: ["llmux-sidecar-example"]),
    ],
    targets: [
        .target(name: "LLMux"),
        .executableTarget(name: "llmux-direct-example", dependencies: ["LLMux"]),
        .executableTarget(name: "llmux-sidecar-example", dependencies: ["LLMux"]),
        .testTarget(name: "LLMuxTests", dependencies: ["LLMux"]),
    ]
)
