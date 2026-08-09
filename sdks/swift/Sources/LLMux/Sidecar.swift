// Sidecar mode: `llmux serve` as a supervised child process.
//
// The SDK spawns it, waits for it, and stops it, so the user never runs a
// server by hand.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// A failure starting or talking to the sidecar.
public enum SidecarError: Error, CustomStringConvertible {
    case binaryNotFound
    case spawn(String)
    case notHealthy(String)
    /// A non-200 response. Carries the code and the body, because the body is
    /// where llmux puts its OpenAI-shaped error object.
    case status(Int, String)
    case transport(String)

    public var description: String {
        switch self {
        case .binaryNotFound:
            return """
                llmux binary not found. Set LLMUX_BINARY, put `llmux` on PATH, or build it: \
                `go build -o sdks/swift/bin/llmux ./cmd/llmux`
                """
        case .spawn(let m): return "failed to spawn llmux: \(m)"
        case .notHealthy(let m): return "llmux did not become healthy: \(m)"
        case .status(let c, let b): return "HTTP \(c): \(b)"
        case .transport(let m): return "transport: \(m)"
        }
    }
}

/// A running `llmux serve` owned by this program.
///
/// **`deinit` stops the child.** Swift has no `defer`-at-scope-exit for object
/// lifetime, but ARC gives the same guarantee: drop the last reference — on the
/// happy path or on a `throw` — and the process is terminated and reaped.
public final class Sidecar: @unchecked Sendable {
    /// `http://127.0.0.1:<port>`.
    public let baseURL: String
    /// `http://127.0.0.1:<port>/v1`, for OpenAI-compatible clients.
    public var openAIBaseURL: String { baseURL + "/v1" }

    private let process: Process
    private let session: URLSession

    /// Spawns the child on a free loopback port and blocks until `/health`
    /// answers 200. On any failure it terminates whatever it started.
    ///
    /// - Parameters:
    ///   - binary: path to the llmux binary. `nil` resolves `$LLMUX_BINARY`,
    ///     then `bin/llmux` beside the package, then `llmux` on `PATH`.
    ///   - configPath: an `llmux.json` for the child.
    ///   - timeout: how long to wait for health.
    public init(
        binary: String? = nil,
        configPath: String? = nil,
        timeout: TimeInterval = 15
    ) throws {
        let bin = try binary ?? Sidecar.resolveBinary()
        let port = try Sidecar.freePort()
        let addr = "127.0.0.1:\(port)"
        baseURL = "http://" + addr

        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 60
        session = URLSession(configuration: cfg)

        process = Process()
        process.executableURL = URL(fileURLWithPath: bin)
        process.arguments = ["serve"]
        var env = ProcessInfo.processInfo.environment
        env["LLMUX_ADDR"] = addr
        if let configPath { env["LLMUX_CONFIG"] = configPath }
        process.environment = env
        // The child's logs are the operator's, not ours to swallow.
        process.standardOutput = FileHandle.standardError
        process.standardError = FileHandle.standardError

        do {
            try process.run()
        } catch {
            throw SidecarError.spawn(error.localizedDescription)
        }

        do {
            try waitHealthy(timeout: timeout)
        } catch {
            // deinit would do this too; doing it here keeps the failure path
            // obvious rather than relying on a reader knowing that it would.
            stop()
            throw error
        }
    }

    deinit { stop() }

    /// Terminates and reaps the child. Idempotent.
    public func stop() {
        guard process.isRunning else { return }
        process.terminate()
        process.waitUntilExit()
    }

    private func waitHealthy(timeout: TimeInterval) throws {
        let deadline = Date().addingTimeInterval(timeout)
        var last = "connection refused"
        while Date() < deadline {
            if !process.isRunning {
                throw SidecarError.notHealthy(
                    "child exited with status \(process.terminationStatus)")
            }
            do {
                _ = try get("/health")
                return
            } catch let e as SidecarError {
                last = e.description
            } catch {
                last = error.localizedDescription
            }
            Thread.sleep(forTimeInterval: 0.05)
        }
        throw SidecarError.notHealthy(last)
    }

    // MARK: HTTP

    /// `GET path`, returning the body and throwing on any non-200.
    public func get(_ path: String) throws -> String {
        var req = URLRequest(url: URL(string: baseURL + path)!)
        req.httpMethod = "GET"
        return try send(req)
    }

    /// `POST path` with a JSON body, returning the body and throwing on any
    /// non-200.
    public func postJSON(_ path: String, _ body: String) throws -> String {
        var req = URLRequest(url: URL(string: baseURL + path)!)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = Data(body.utf8)
        return try send(req)
    }

    /// `POST /v1/chat/completions`, non-streaming.
    public func chat(_ requestJSON: String) throws -> String {
        try postJSON("/v1/chat/completions", requestJSON)
    }

    /// `GET /v1/models`.
    public func models() throws -> String {
        try get("/v1/models")
    }

    /// Streams `POST /v1/chat/completions` as an `AsyncSequence` of chunk JSON
    /// documents, decoded from SSE.
    ///
    /// This is the sidecar's answer to `llmux_stream`, and the reason a host
    /// that cannot or will not load a shared library still has an honest
    /// streaming path — one where time-to-first-token is real, rather than a
    /// binding that buffers the whole answer and replays it as fake chunks.
    ///
    /// `URLSession.bytes(for:)` gives real backpressure here, which the direct
    /// path's `AsyncThrowingStream` does not: the async `for` loop drives the
    /// socket read.
    public func chatStream(_ requestJSON: String) -> AsyncThrowingStream<String, Error> {
        AsyncThrowingStream(String.self, bufferingPolicy: .unbounded) { continuation in
            let task = Task {
                do {
                    var req = URLRequest(url: URL(string: baseURL + "/v1/chat/completions")!)
                    req.httpMethod = "POST"
                    req.setValue("application/json", forHTTPHeaderField: "Content-Type")
                    req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    req.httpBody = Data(requestJSON.utf8)

                    let (bytes, response) = try await session.bytes(for: req)
                    if let http = response as? HTTPURLResponse, http.statusCode != 200 {
                        var body = ""
                        for try await line in bytes.lines { body += line }
                        throw SidecarError.status(http.statusCode, body)
                    }
                    for try await line in bytes.lines {
                        if Task.isCancelled { break }
                        let trimmed = line.trimmingCharacters(in: .whitespaces)
                        guard trimmed.hasPrefix("data:") else { continue }
                        let payload = String(trimmed.dropFirst(5))
                            .trimmingCharacters(in: .whitespaces)
                        if payload == "[DONE]" { break }
                        continuation.yield(payload)
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    /// A synchronous `URLSession` round trip.
    ///
    /// `URLSession` is callback-based and this SDK's non-streaming surface is
    /// synchronous, so a semaphore bridges them. That is safe **here** because
    /// the completion runs on a `URLSession` delegate queue, never on the
    /// thread being blocked — but do not copy this pattern into code running on
    /// Swift's cooperative pool, where blocking a thread can deadlock.
    private func send(_ req: URLRequest) throws -> String {
        var result: Result<String, Error>!
        let sem = DispatchSemaphore(value: 0)
        session.dataTask(with: req) { data, response, error in
            defer { sem.signal() }
            if let error {
                result = .failure(SidecarError.transport(error.localizedDescription))
                return
            }
            let body = String(data: data ?? Data(), encoding: .utf8) ?? ""
            if let http = response as? HTTPURLResponse, http.statusCode != 200 {
                result = .failure(SidecarError.status(http.statusCode, body))
                return
            }
            result = .success(body)
        }.resume()
        sem.wait()
        return try result.get()
    }

    // MARK: Resolution

    static func resolveBinary() throws -> String {
        let env = ProcessInfo.processInfo.environment
        if let explicit = env["LLMUX_BINARY"], !explicit.isEmpty { return explicit }

        let name = "llmux"
        // #filePath is Sources/LLMux/Sidecar.swift; the package root is 3 up.
        let packageRoot = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let bundled = packageRoot.appendingPathComponent("bin").appendingPathComponent(name).path
        if FileManager.default.fileExists(atPath: bundled) { return bundled }

        for dir in (env["PATH"] ?? "").split(separator: ":") {
            let candidate = String(dir) + "/" + name
            if FileManager.default.isExecutableFile(atPath: candidate) { return candidate }
        }
        throw SidecarError.binaryNotFound
    }

    /// Binds port 0, reads the port the kernel picked, and releases it.
    ///
    /// Inherently racy — something else can take the port between the close and
    /// the child's bind. It is the same race every "find a free port" helper
    /// has, including the Go and Rust SDKs'; the alternative is passing the
    /// listening socket to the child, which llmux does not support.
    static func freePort() throws -> UInt16 {
        let fd = socket(AF_INET, SOCK_STREAM, 0)
        guard fd >= 0 else { throw SidecarError.spawn("socket() failed") }
        defer { Foundation.close(fd) }
        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = 0
        addr.sin_addr.s_addr = inet_addr("127.0.0.1")
        let bound = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                bind(fd, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bound == 0 else { throw SidecarError.spawn("bind() failed") }
        var out = sockaddr_in()
        var len = socklen_t(MemoryLayout<sockaddr_in>.size)
        let got = withUnsafeMutablePointer(to: &out) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                getsockname(fd, $0, &len)
            }
        }
        guard got == 0 else { throw SidecarError.spawn("getsockname() failed") }
        return UInt16(bigEndian: out.sin_port)
    }
}
