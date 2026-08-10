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

// MARK: - Cancellation

/// Fact 4 from the FFI contract, exercised directly: cancelling a handle with
/// nothing running is a no-op, cancelling twice is a no-op, and neither
/// leaves the handle any less usable afterwards. Needs a real `libllmux` but
/// no upstream at all, so it is cheap enough to always run once gated.
@Test func cancelIsANoOpWithNothingInFlight() throws {
    guard let path = library() else { return }
    let gw = try Gateway(libraryPath: path)
    gw.cancel()
    gw.cancel()
    #expect(!(try gw.models().isEmpty))
}

/// A minimal spawn of `sdks/fake-upstream.py` — the harness written for
/// exactly this question: when a consumer cancels a stream, how many chunks
/// did the provider actually produce? See that file's module doc for why an
/// upstream has to answer this itself, out of band, rather than the SDK
/// trying to infer it from what it received.
///
/// Modeled on `Sidecar`'s child-process handling in this same package, with
/// one difference: it polls a redirected log file for the `URL `/`CONFIG `
/// lines rather than reading a live `Pipe`, the way `run.sh` polls the Go
/// fake's log for the same reason — a `Pipe` nobody is draining can deadlock
/// the child on its own stdout once the kernel buffer fills, and a polled
/// file never can.
private struct FakeUpstream {
    let process: Process
    let baseURL: String
    let configJSON: String

    /// `nil` when this checkout cannot run the harness (no `python3` on
    /// `PATH`, or `sdks/fake-upstream.py` is not where expected) — a normal
    /// state on a machine that has never set up llmux's Python tooling, and
    /// distinct from an actual test failure.
    static func start(text: String, chunkDelayMs: Int) throws -> FakeUpstream? {
        guard let python = which("python3") else { return nil }
        // #filePath is Tests/LLMuxTests/DirectTests.swift; fake-upstream.py is
        // shared by every non-JS SDK, one level above the package root at
        // sdks/swift.
        let script = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()  // LLMuxTests/
            .deletingLastPathComponent()  // Tests/
            .deletingLastPathComponent()  // sdks/swift/
            .deletingLastPathComponent()  // sdks/
            .appendingPathComponent("fake-upstream.py")
        guard FileManager.default.fileExists(atPath: script.path) else { return nil }

        let logPath = NSTemporaryDirectory() + "llmux-swift-fakeupstream-\(UUID().uuidString).log"
        FileManager.default.createFile(atPath: logPath, contents: nil)
        let log = try FileHandle(forWritingTo: URL(fileURLWithPath: logPath))

        let p = Process()
        p.executableURL = URL(fileURLWithPath: python)
        p.arguments = [script.path, "--text", text, "--chunk-delay-ms", String(chunkDelayMs)]
        p.standardOutput = log
        p.standardError = log
        try p.run()

        var urlLine: String?
        var configLine: String?
        let deadline = Date().addingTimeInterval(5)
        while Date() < deadline {
            if let contents = try? String(contentsOfFile: logPath, encoding: .utf8) {
                for line in contents.split(separator: "\n", omittingEmptySubsequences: true) {
                    if line.hasPrefix("URL ") { urlLine = String(line.dropFirst(4)) }
                    if line.hasPrefix("CONFIG ") { configLine = String(line.dropFirst(7)) }
                }
            }
            if urlLine != nil && configLine != nil { break }
            Thread.sleep(forTimeInterval: 0.02)
        }
        try? log.close()
        try? FileManager.default.removeItem(atPath: logPath)

        guard let baseURL = urlLine, let configJSON = configLine else {
            p.terminate()
            return nil
        }
        return FakeUpstream(process: p, baseURL: baseURL, configJSON: configJSON)
    }

    /// `GET /generated`: `{"generated": N, "streams": M, "disconnects": D}`,
    /// counting only chunks the upstream actually wrote to a socket.
    func generated() throws -> Int {
        let data = try Data(contentsOf: URL(string: baseURL + "/generated")!)
        let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any]
        return (obj?["generated"] as? Int) ?? -1
    }

    func stop() {
        process.terminate()
        process.waitUntilExit()
    }
}

private func which(_ name: String) -> String? {
    for dir in (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":") {
        let candidate = String(dir) + "/" + name
        if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
    }
    return nil
}

/// Ten words, generated 20 ms apart — slow enough that breaking out after 3
/// chunks happens while the other 7 are still unmade.
private let cancellationText = "one two three four five six seven eight nine ten"

/// The regression this feature exists to fix: a consumer that breaks out of
/// `chunks(_:)` early must reach `llmux_cancel`, and that must actually stop
/// the *upstream provider* — not just stop this process from reading more of
/// it, which is indistinguishable from the outside but leaves the full
/// generation billed regardless.
///
/// This is fact 5 from the FFI contract (a completed stream generates 12
/// chunks for 10 words — the words plus a finish-reason chunk plus a usage
/// chunk), reproduced here as an assertion instead of only in a README.
@Test func breakingOutOfChunksStopsTheUpstream() async throws {
    guard let path = library() else { return }
    guard let fake = try FakeUpstream.start(text: cancellationText, chunkDelayMs: 20) else {
        print("no python3 / sdks/fake-upstream.py — cancellation test SKIPPED")
        return
    }
    defer { fake.stop() }

    let gw = try Gateway(libraryPath: path, configJSON: fake.configJSON)
    let reqJSON = """
        {"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}
        """

    var seen = 0
    for try await _ in gw.chunks(reqJSON) {
        seen += 1
        if seen == 3 { break }
    }
    #expect(seen == 3)

    // cancel() fires from `onTermination` off this thread, and the upstream
    // needs a moment to notice its connection died. Poll rather than sleep a
    // fixed amount, and stop as soon as the count is plausibly final.
    var generated = -1
    for _ in 0..<50 {
        generated = try fake.generated()
        if generated <= seen + 2 { break }
        try await Task.sleep(nanoseconds: 50_000_000)
    }
    #expect(generated < 12, "cancellation did not reach the upstream: generated \(generated) of 12")
}

/// The other half of the same fix: cancelling the *consuming `Task`* — not
/// just breaking a loop — must also reach `llmux_cancel` and stop the
/// upstream.
///
/// This does **not** assert on a thrown error, and that omission is
/// deliberate, not an oversight: `AsyncThrowingStream`'s documented
/// cancellation behavior is that a `next()` call suspended when the
/// *consuming* task is cancelled resolves to `nil` — ending the loop exactly
/// as if the sequence had finished normally — rather than surfacing whatever
/// the producer eventually finishes with. (An earlier version of this test
/// asserted `CancellationError` here and failed for exactly that reason: the
/// producer's `continuation.finish(throwing: CancellationError())` never gets
/// read back out, because the consumer's own `next()` already returned via
/// the cancellation fast path.) That is standard library behavior, not
/// something this SDK can or should override — see
/// ``explicitCancelOfAnActiveStreamThrowsCancellationError`` below for where
/// the translated error genuinely is observable: a call cancelled by
/// something *other than the consuming task's own cancellation*.
@Test func taskCancellationStopsTheUpstream() async throws {
    guard let path = library() else { return }
    guard let fake = try FakeUpstream.start(text: cancellationText, chunkDelayMs: 20) else {
        print("no python3 / sdks/fake-upstream.py — cancellation test SKIPPED")
        return
    }
    defer { fake.stop() }

    let gw = try Gateway(libraryPath: path, configJSON: fake.configJSON)
    let reqJSON = """
        {"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}
        """

    // An actor, not a plain var, because it is written from the task below
    // and read from this test function concurrently.
    actor Progress {
        private(set) var chunkCount = 0
        func sawChunk() { chunkCount += 1 }
    }
    let progress = Progress()

    let task = Task {
        for try await _ in gw.chunks(reqJSON) {
            await progress.sawChunk()
        }
    }

    let deadline = Date().addingTimeInterval(5)
    while await progress.chunkCount < 2, Date() < deadline {
        try await Task.sleep(nanoseconds: 5_000_000)
    }
    #expect(await progress.chunkCount >= 2, "never saw 2 chunks before the deadline")
    task.cancel()
    _ = try? await task.value

    var generated = -1
    for _ in 0..<50 {
        generated = try fake.generated()
        if generated <= 6 { break }
        try await Task.sleep(nanoseconds: 50_000_000)
    }
    #expect(generated < 12, "Task cancellation did not reach the upstream: generated \(generated) of 12")
}

/// Where the `CancellationError` translation in `chunks(_:)` is actually
/// observable: `Gateway.cancel()` invoked from outside, while the consuming
/// `Task` itself is never cancelled and so does not take the "cancelled task
/// returns nil" fast path documented above. Here `next()` has nothing to
/// short-circuit on, so it genuinely delivers what the producer finished
/// with — and that must be Swift's `CancellationError`, not llmux's raw
/// `context canceled` string.
///
/// This also doubles as the low-level `cancel()` contract from the FFI
/// header: safe to call from another thread while a call on the same handle
/// is blocked.
@Test func explicitCancelOfAnActiveStreamThrowsCancellationError() async throws {
    guard let path = library() else { return }
    guard let fake = try FakeUpstream.start(text: cancellationText, chunkDelayMs: 20) else {
        print("no python3 / sdks/fake-upstream.py — cancellation test SKIPPED")
        return
    }
    defer { fake.stop() }

    let gw = try Gateway(libraryPath: path, configJSON: fake.configJSON)
    let reqJSON = """
        {"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}
        """

    var seen = 0
    do {
        for try await _ in gw.chunks(reqJSON) {
            seen += 1
            if seen == 2 {
                // Fired from a detached task so it genuinely races the
                // background queue driving this same stream, the way an
                // unrelated part of a real program would call it.
                Task.detached { gw.cancel() }
            }
        }
        Issue.record("expected gw.cancel() to end the stream with an error")
    } catch {
        #expect(error is CancellationError, "expected CancellationError, got \(String(describing: error))")
    }
    #expect(seen >= 2)
}
