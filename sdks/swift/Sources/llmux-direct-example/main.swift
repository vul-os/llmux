// llmux in this process, over the C ABI.
//
//   swift run llmux-direct-example                 # needs provider keys
//   ./sdks/swift/run.sh direct                     # offline, ffi/fakeupstream
//
// Environment:
//   LLMUX_LIBRARY      path to libllmux.dylib / .so (else it is searched for)
//   LLMUX_CONFIG_JSON  an llmux config document; unset means defaults + env
//   LLMUX_MODEL        model to route (default "demo")

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
