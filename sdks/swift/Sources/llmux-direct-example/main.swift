// llmux in this process, over the C ABI.
//
//   swift run llmux-direct-example                 # needs provider keys
//   ./sdks/swift/run.sh direct                     # offline, ffi/fakeupstream
//
// Environment:
//   LLMUX_LIBRARY      path to libllmux.dylib / .so (else it is searched for)
//   LLMUX_CONFIG_JSON  an llmux config document; unset means defaults + env
//   LLMUX_MODEL        model to route (default "demo")
//
// The cancellation section near the end spawns its own second upstream —
// `sdks/fake-upstream.py`, which needs `python3` on PATH — rather than
// depending on whatever LLMUX_CONFIG_JSON points at. That upstream (usually
// `ffi/fakeupstream`, a Go fake with no configurable delay) answers instantly,
// which is exactly wrong for demonstrating a cancellation landing mid-stream:
// there needs to be time between chunks for a `Task.cancel()` to land in.

import Foundation
import LLMux

let env = ProcessInfo.processInfo.environment
let model = env["LLMUX_MODEL"] ?? "demo"
let configJSON = env["LLMUX_CONFIG_JSON"]

func jsonString(_ s: String) -> String {
    // Enough for a model name. Anything real should use JSONEncoder.
    let escaped = s.replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
    return "\"\(escaped)\""
}

/// Pulls `choices[0].delta.content` out of a chunk with JSONSerialization.
func deltaContent(_ chunk: String) -> String {
    guard
        let data = chunk.data(using: .utf8),
        let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
        let choices = obj["choices"] as? [[String: Any]],
        let delta = choices.first?["delta"] as? [String: Any],
        let text = delta["content"] as? String
    else { return "" }
    return text
}

func first(_ s: String, _ n: Int) -> String {
    s.count <= n ? s : String(s.prefix(n)) + "…"
}

// MARK: - The cancellation harness

/// A running `sdks/fake-upstream.py`: the one upstream in this repo that
/// sleeps a configurable amount before each chunk and can answer, after the
/// fact, exactly how many chunks it wrote to a socket. See that file's module
/// doc for why a cancellation demo needs an upstream willing to tell on
/// itself, rather than trusting the client's own chunk count.
struct FakeUpstream {
    let process: Process
    let baseURL: String
    let configJSON: String

    /// `GET /generated`: `{"generated": N, "streams": M, "disconnects": D}`.
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

/// Spawns `sdks/fake-upstream.py --text text --chunk-delay-ms chunkDelayMs`
/// and waits for it to report ready.
///
/// Redirects stdout to a temp file and polls it rather than reading a `Pipe`
/// live: a `Pipe` nobody drains can deadlock the child on its own stdout once
/// the kernel buffer fills, and a polled file cannot.
func startFakeUpstream(text: String, chunkDelayMs: Int) throws -> FakeUpstream {
    guard let python = which("python3") else {
        throw LLMuxError.llmux("python3 not found on PATH — needed for sdks/fake-upstream.py")
    }
    // This file is Sources/llmux-direct-example/main.swift; fake-upstream.py
    // is shared by every non-JS SDK, two levels up in sdks/.
    let script = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()  // llmux-direct-example/
        .deletingLastPathComponent()  // Sources/
        .deletingLastPathComponent()  // sdks/swift/
        .deletingLastPathComponent()  // sdks/
        .appendingPathComponent("fake-upstream.py")
    guard FileManager.default.fileExists(atPath: script.path) else {
        throw LLMuxError.llmux("fake-upstream.py not found at \(script.path)")
    }

    let logPath = NSTemporaryDirectory() + "llmux-swift-example-fakeupstream-\(UUID().uuidString).log"
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
        throw LLMuxError.llmux("fake-upstream.py never printed CONFIG/URL within 5s")
    }
    return FakeUpstream(process: p, baseURL: baseURL, configJSON: configJSON)
}

func which(_ name: String) -> String? {
    for dir in (ProcessInfo.processInfo.environment["PATH"] ?? "").split(separator: ":") {
        let candidate = String(dir) + "/" + name
        if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
    }
    return nil
}

do {
    let path = try LibraryLocator.find()
    print("library: \(path)")

    let gw = try Gateway(libraryPath: path, configJSON: configJSON)
    // The handle is owned by `gw` and released in deinit — at the end of this
    // scope, and on every `throw` below. ARC is the RAII here; there is no
    // close() to forget and no defer to write.
    print("abi:     \(gw.abiVersion)")

    // In production, refuse a mismatch instead:
    //     let gw = try Gateway(expectedVersion: "0.1.2", configJSON: configJSON)
    // A shared library resolves off a load path you may not control.

    // ------------------------------------------------------------------ models
    // Answered from memory — no upstream is contacted.
    var t = Date()
    let models = try gw.models()
    print("models:  \(models.count) bytes in \(ms(since: t))")

    // -------------------------------------------------------------------- chat
    // The same JSON the HTTP API uses. A body that works against
    // POST /v1/chat/completions works here unchanged.
    let chatReq = """
        {"model":\(jsonString(model)),"messages":[{"role":"user","content":"count to four"}]}
        """
    t = Date()
    let chat = try gw.chat(chatReq)
    print("chat:    \(ms(since: t))")
    print("         \(first(chat, 200))")

    // A "chat" with "stream": true is REFUSED rather than quietly served as one
    // blob after the fact. Demonstrating that beats describing it.
    let streamReq = """
        {"model":\(jsonString(model)),"messages":[{"role":"user","content":"hi"}],"stream":true}
        """
    do {
        _ = try gw.chat(streamReq)
        print("refusal: NOT refused — that would be a bug in llmux")
    } catch {
        print("refusal: \(error)")
    }

    // ------------------------------------------------------------------ stream
    // The idiomatic Swift shape: an AsyncSequence.
    print("stream:  ", terminator: "")
    t = Date()
    var firstToken: String?
    var count = 0
    for try await chunk in gw.chunks(streamReq) {
        count += 1
        if firstToken == nil { firstToken = ms(since: t) }
        print(deltaContent(chunk), terminator: "")
    }
    print()
    print("         \(count) chunks, first at \(firstToken ?? "-"), total \(ms(since: t))")

    // ------------------------------------------------------- early termination
    // Breaking out of the loop terminates the AsyncSequence, which sets the
    // stream's termination flag, which makes the C callback return non-zero,
    // which stops the stream at the library. No orphan work.
    var seen = 0
    for try await _ in gw.chunks(streamReq) {
        seen += 1
        if seen == 2 { break }
    }
    print("early:   broke out after \(seen) chunk(s) — stopping is not an error")

    // The callback form, for completeness. Returning false stops the stream and
    // still returns normally.
    var syncSeen = 0
    try gw.streamSync(streamReq) { _ in
        syncSeen += 1
        return syncSeen < 3
    }
    print("sync:    stopped after \(syncSeen) chunk(s)")

    // ------------------------------------------------------------ cancellation
    // llmux_cancel (v0.1.5) is the only way to abandon a call that is already
    // blocked inside llmux_stream — a chunk callback returning non-zero only
    // helps once a chunk arrives to run it, and an idle upstream may not send
    // one for a while. Gateway.cancel() reaches in from another thread; the
    // AsyncThrowingStream from chunks(_:) wires it to onTermination, so
    // abandoning the sequence — by breaking, or (as here) by cancelling the
    // Task consuming it — reaches llmux_cancel without the caller doing
    // anything extra.
    //
    // This needs its own upstream: ffi/fakeupstream answers instantly, which
    // leaves no window for a cancellation to land mid-stream. sdks/fake-
    // upstream.py sleeps between chunks and counts, out of band, exactly how
    // many it wrote to a socket before the connection died — the only way to
    // tell a binding that truly stopped the provider from one that only
    // stopped reading it locally while the provider (and the meter) ran on.
    do {
        let cancellationWords = "one two three four five six seven eight nine ten"
        let harness = try startFakeUpstream(text: cancellationWords, chunkDelayMs: 100)
        defer { harness.stop() }

        let cancelGW = try Gateway(libraryPath: path, configJSON: harness.configJSON)
        let cancelReq = """
            {"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}
            """

        actor Counter {
            private(set) var n = 0
            func increment() { n += 1 }
        }
        let counter = Counter()
        let consumer = Task {
            for try await _ in cancelGW.chunks(cancelReq) {
                await counter.increment()
            }
        }
        while await counter.n < 3 { try await Task.sleep(nanoseconds: 5_000_000) }
        consumer.cancel()
        _ = try? await consumer.value  // AsyncThrowingStream ends silently on
        // self-cancellation (see Direct.swift's chunks(_:) doc) — nothing to
        // catch here, the interesting number is what the upstream saw.

        let cancelSeen = await counter.n
        print("cancel:  consumer stopped at \(cancelSeen) chunk(s) (Task cancelled)")

        // onTermination's cancel() already fired synchronously; the upstream
        // still needs a moment to notice its socket died before /generated
        // reflects it, so poll briefly instead of asserting on the instant.
        var generated = -1
        for _ in 0..<50 {
            generated = try harness.generated()
            if generated <= cancelSeen + 2 { break }
            try await Task.sleep(nanoseconds: 50_000_000)
        }
        print("         upstream generated \(generated) of 12 (10 words + finish + usage chunk)")
    } catch {
        print("cancel:  SKIPPED (\(error))")
    }

    // ------------------------------------------------------------ error path
    // An unknown method is a clean error string, not a crash, and the message
    // it allocated was freed before this line runs.
    do {
        _ = try gw.call("no-such-method", "{}")
        print("bogus:   unexpectedly succeeded")
    } catch {
        print("bogus:   \(error)")
    }
    // gw goes out of scope here: llmux_close(handle).
} catch {
    FileHandle.standardError.write(Data("error: \(error)\n".utf8))
    exit(1)
}

func ms(since: Date) -> String {
    String(format: "%.3fms", Date().timeIntervalSince(since) * 1000)
}
