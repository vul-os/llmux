// Tests for the direct (C ABI) path.
//
// # Why swift-testing and not XCTest
//
// **XCTest ships with Xcode, not with the Command Line Tools.** On a machine
// with only the CLT installed — which is where this SDK was written and run —
// `import XCTest` fails with "no such module 'XCTest'" and `swift test` cannot
// build at all. swift-testing (`import Testing`) is part of the Swift 6
// toolchain itself, so it works with the CLT alone.
//
// That is a real constraint on anyone packaging a Swift SDK, and worth knowing
// before you write a suite that only runs on machines with a 10 GB IDE.
//
// # Gating
//
// The tests that need a real `libllmux` are gated on it being present: without
// it they return rather than fail, because a checkout that has not run
// `scripts/build-ffi.sh` is a normal state. Gating creates the classic false
// green — a suite that skips everything and reports success — so
// `gateIsHonestAboutSkipping` prints which way it went.

import Foundation
import Testing

@testable import LLMux

/// The library, or nil if this checkout has not built one.
///
/// `LibraryLocator.find()` walks up from the CURRENT DIRECTORY, which under
/// `swift test` is the package directory, so `dist/ffi/…` two levels up is
/// reachable. `$LLMUX_LIBRARY` overrides it either way.
private func library() -> String? {
    try? LibraryLocator.find()
}

@Test func gateIsHonestAboutSkipping() {
    if let p = library() {
        print("libllmux found at \(p) — direct tests RAN")
    } else {
        print("no libllmux — direct tests SKIPPED (run scripts/build-ffi.sh)")
    }
}

@Test func fileNameMatchesThisPlatform() {
    #if os(macOS)
    #expect(LibraryLocator.fileName == "libllmux.dylib")
    #elseif os(Windows)
    #expect(LibraryLocator.fileName == "llmux.dll")
    #else
    #expect(LibraryLocator.fileName == "libllmux.so")
    #endif
}

@Test func missingLibraryIsALoadErrorNotACrash() {
    #expect(throws: LLMuxError.self) {
        _ = try Gateway(libraryPath: "/nonexistent/libllmux.dylib")
    }
    do {
        _ = try Gateway(libraryPath: "/nonexistent/libllmux.dylib")
        Issue.record("expected a throw")
    } catch let e as LLMuxError {
        guard case .load = e else {
            Issue.record("expected .load, got \(e)")
            return
        }
        // The message names the path, which is the difference between a
        // debuggable error and "loading libllmux: image not found".
        #expect(e.description.contains("/nonexistent/libllmux.dylib"))
    } catch {
        Issue.record("unexpected error \(error)")
    }
}

@Test func libraryNotFoundNamesTheEnvVarAndThePaths() {
    let e = LLMuxError.libraryNotFound(["/a/libllmux.so"])
    #expect(e.description.contains("LLMUX_LIBRARY"))
    #expect(e.description.contains("/a/libllmux.so"))
}

@Test func opensReportsAVersionAndCloses() throws {
    guard let path = library() else { return }
    let gw = try Gateway(libraryPath: path)
    let v = gw.abiVersion
    #expect(!v.isEmpty)
    #expect(v.first?.isNumber == true, "unexpected version \(v)")
    // gw deinits at the end of this scope: llmux_close.
}

@Test func versionMismatchIsDetected() throws {
    guard library() != nil else { return }
    do {
        _ = try Gateway(expectedVersion: "0.0.0-not-real")
        Issue.record("a bogus expected version was accepted")
    } catch let LLMuxError.versionMismatch(loaded, expected) {
        #expect(expected == "0.0.0-not-real")
        #expect(!loaded.isEmpty)
    }
}

@Test func modelsIsAnsweredFromMemory() throws {
    guard let path = library() else { return }
    let gw = try Gateway(libraryPath: path)
    // No upstream is configured and none is needed.
    let json = try gw.models()
    #expect(json.contains("\"object\":\"list\""), "\(json)")
}

@Test func unknownMethodIsACleanErrorNotACrash() throws {
    guard let path = library() else { return }
    let gw = try Gateway(libraryPath: path)
    do {
        _ = try gw.call("definitely-not-a-method", "{}")
        Issue.record("an unknown method was accepted")
    } catch {
        #expect("\(error)".contains("unknown method"), "\(error)")
    }
    // The message was malloc'd by the library and freed by takeError. If it
    // were not, this test would still pass — which is why leak checking belongs
    // in ffi/ctest/smoke.c, not here.
}

/// Regression guard, inherited from the Rust binding for the same library.
///
/// That binding unloaded the library with each handle, and a 200-cycle loop
/// **hung** — `dlclose` on a Go `c-shared` object unmaps code the Go runtime's
/// threads are still executing. `Library.shared(at:)` loads once per process
/// and never unloads, so this is fast.
@Test func manyOpenCloseCyclesStayFast() throws {
    guard let path = library() else { return }
    for _ in 0..<200 {
        let gw = try Gateway(libraryPath: path)
        _ = try gw.models()
    }
}

/// Two gateways at once must be independent handles, not a shared singleton.
/// The *library* is shared; the *handles* are not.
@Test func twoGatewaysCoexist() throws {
    guard let path = library() else { return }
    let a = try Gateway(libraryPath: path)
    let b = try Gateway(libraryPath: path)
    #expect(!(try a.models().isEmpty))
    #expect(!(try b.models().isEmpty))
}

/// `freePort` underpins the sidecar. Assert it returns something usable rather
/// than assuming it does.
@Test func freePortIsInRange() throws {
    let p = try Sidecar.freePort()
    #expect(p > 0)
}
