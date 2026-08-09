// llmux as a supervised child process, over HTTP.
//
//   swift run llmux-sidecar-example        # needs provider keys
//   ./sdks/swift/run.sh sidecar            # offline, ffi/fakeupstream
//
// Environment:
//   LLMUX_BINARY  path to the llmux binary (else bin/llmux, else PATH)
//   LLMUX_CONFIG  path to an llmux.json for the child
//   LLMUX_MODEL   model to route (default "demo")
//
// Compare with llmux-direct-example, which is the better default for a Swift
// program on darwin/arm64. This is the right choice when you need per-tenant
// keys and budgets, when several processes should share one gateway, or when
// you are on a platform with no prebuilt shared library — which for Swift means
// anything that is not darwin/arm64 or linux/arm64.

import Foundation
import LLMux

let env = ProcessInfo.processInfo.environment
let model = env["LLMUX_MODEL"] ?? "demo"

func jsonString(_ s: String) -> String {
    let escaped = s.replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"")
    return "\"\(escaped)\""
}

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

func ms(since: Date) -> String {
    String(format: "%.3fms", Date().timeIntervalSince(since) * 1000)
}

do {
    // Spawns the child on a free loopback port and blocks until /health is 200.
    // On failure it terminates whatever it started, so this `try` cannot leave
    // a serving llmux behind.
    let sc = try Sidecar(binary: env["LLMUX_BINARY"], configPath: env["LLMUX_CONFIG"])
    // From here the process is owned by `sc` and stopped in deinit — on the
    // error paths below as much as at the end.
    print("sidecar: \(sc.baseURL)")
    print("openai:  \(sc.openAIBaseURL)")

    // ------------------------------------------------------------------ models
    var t = Date()
    let models = try sc.models()
    print("models:  \(models.count) bytes in \(ms(since: t))")

    // -------------------------------------------------------------------- chat
    // Byte for byte the same request document the C ABI takes. One wire
    // contract, two transports.
    let chatReq = """
        {"model":\(jsonString(model)),"messages":[{"role":"user","content":"count to four"}]}
        """
    t = Date()
    let chat = try sc.chat(chatReq)
    print("chat:    \(ms(since: t))")
    print("         \(first(chat, 200))")

    // ------------------------------------------------------------------ stream
    // SSE over URLSession.bytes(for:), not a C callback. This is the honest
    // streaming path for a host that cannot or will not load a shared library —
    // and unlike the direct path's AsyncThrowingStream, the async for-loop here
    // drives the socket read, so backpressure is real.
    let streamReq = """
        {"model":\(jsonString(model)),"messages":[{"role":"user","content":"hi"}],"stream":true}
        """
    print("stream:  ", terminator: "")
    t = Date()
    var firstToken: String?
    var count = 0
    for try await chunk in sc.chatStream(streamReq) {
        count += 1
        if firstToken == nil { firstToken = ms(since: t) }
        print(deltaContent(chunk), terminator: "")
    }
    print()
    print("         \(count) chunks, first at \(firstToken ?? "-"), total \(ms(since: t))")

    // ------------------------------------------------------------ error path
    do {
        _ = try sc.chat(#"{"model":"no-such-model-anywhere","messages":[{"role":"user","content":"x"}]}"#)
        print("bogus:   unexpectedly succeeded")
    } catch {
        // llmux returns an OpenAI-shaped error object in the body, surfaced
        // verbatim rather than paraphrased into a status code.
        print("bogus:   \(first("\(error)", 180))")
    }

    // Explicit, because an example that starts a server and relies on deinit
    // teaches a habit that is fine here and wrong in a long-lived object graph.
    sc.stop()
    print("stopped: child reaped")
} catch {
    FileHandle.standardError.write(Data("error: \(error)\n".utf8))
    exit(1)
}
