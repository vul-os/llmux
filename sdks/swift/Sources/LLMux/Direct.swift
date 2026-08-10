// Direct mode: llmux running inside this process, over the C ABI.
//
// Swift's C interop is one of its genuine strengths, and this file uses the
// plainest form of it: dlopen/dlsym plus @convention(c) function types. No
// module map, no bridging header, no unsafeFlags in Package.swift — which
// matters, because a target with unsafeFlags cannot be depended on by another
// package.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

#if canImport(Darwin)
import Darwin
#else
import Glibc
#endif
import Foundation

// MARK: - Errors

/// Everything that can go wrong on the direct path.
public enum LLMuxError: Error, CustomStringConvertible {
    /// No `libllmux` could be found. Carries the paths that were tried.
    case libraryNotFound([String])
    /// `dlopen` failed. Carries the platform's message.
    case load(String)
    /// A symbol was missing — usually a library that is not llmux, or one from
    /// a different major version.
    case missingSymbol(String)
    /// The loaded library reports a different version than expected.
    case versionMismatch(loaded: String, expected: String)
    /// llmux itself failed. The string is the library's own message, which is
    /// plain UTF-8 text and deliberately **not** JSON — do not parse it.
    case llmux(String)

    public var description: String {
        switch self {
        case .libraryNotFound(let tried):
            return """
                libllmux not found. Set LLMUX_LIBRARY to its path, or build it with \
                `scripts/build-ffi.sh`. Tried: \(tried.joined(separator: ", "))
                """
        case .load(let msg):
            return "loading libllmux: \(msg)"
        case .missingSymbol(let name):
            return "libllmux is missing the symbol \(name) — is this really libllmux?"
        case .versionMismatch(let loaded, let expected):
            return """
                libllmux reports version \(loaded), expected \(expected) — a stale library is \
                earlier on your load path
                """
        case .llmux(let msg):
            return msg
        }
    }
}

// MARK: - The ABI

typealias AbiVersionFn = @convention(c) () -> UnsafePointer<CChar>?
typealias NewFn = @convention(c) (
    UnsafePointer<CChar>?, UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> UInt64
typealias CloseFn = @convention(c) (UInt64) -> Void
typealias CancelFn = @convention(c) (UInt64) -> Void
typealias CallFn = @convention(c) (
    UInt64, UnsafePointer<CChar>?, UnsafePointer<CChar>?,
    UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> UnsafeMutablePointer<CChar>?
typealias FreeFn = @convention(c) (UnsafeMutablePointer<CChar>?) -> Void
typealias ChunkCb = @convention(c) (UnsafePointer<CChar>?, UnsafeMutableRawPointer?) -> Int32
typealias StreamFn = @convention(c) (
    UInt64, UnsafePointer<CChar>?, UnsafePointer<CChar>?, ChunkCb?, UnsafeMutableRawPointer?,
    UnsafeMutablePointer<UnsafeMutablePointer<CChar>?>?
) -> Int32

/// The loaded library and its seven resolved symbols.
///
/// # Why this is never unloaded
///
/// There is no `dlclose` anywhere in this file, and that is deliberate.
/// `libllmux` is a Go `c-shared` object: loading it starts the Go runtime and
/// its threads, and Go has no way to shut that down, so unloading would unmap
/// code those threads are still executing.
///
/// The Rust binding for this same library was written the other way first —
/// unloading with each handle — and a 200-cycle open/close loop hung and had to
/// be killed. `Library.shared(at:)` caches one instance per path for the life
/// of the process.
final class Library: @unchecked Sendable {
    let abiVersion: AbiVersionFn
    let new: NewFn
    let close: CloseFn
    let cancel: CancelFn
    let call: CallFn
    let free: FreeFn
    let stream: StreamFn

    private static let lock = NSLock()
    private static var loaded: [String: Library] = [:]

    /// The process-wide `Library` for `path`, loading it on first use.
    static func shared(at path: String) throws -> Library {
        lock.lock()
        defer { lock.unlock() }
        if let existing = loaded[path] { return existing }
        let lib = try Library(path: path)
        loaded[path] = lib
        return lib
    }

    private init(path: String) throws {
        guard let handle = dlopen(path, RTLD_NOW | RTLD_LOCAL) else {
            let reason = dlerror().map { String(cString: $0) } ?? "unknown error"
            throw LLMuxError.load("\(path): \(reason)")
        }
        // Deliberately not stored: nothing may ever dlclose it. See above.
        func sym<T>(_ name: String, _ type: T.Type) throws -> T {
            guard let p = dlsym(handle, name) else { throw LLMuxError.missingSymbol(name) }
            return unsafeBitCast(p, to: type)
        }
        abiVersion = try sym("llmux_abi_version", AbiVersionFn.self)
        new = try sym("llmux_new", NewFn.self)
        close = try sym("llmux_close", CloseFn.self)
        cancel = try sym("llmux_cancel", CancelFn.self)
        call = try sym("llmux_call", CallFn.self)
        free = try sym("llmux_free", FreeFn.self)
        stream = try sym("llmux_stream", StreamFn.self)
    }

    var version: String {
        // llmux_abi_version returns a STATIC string owned by the library. It
        // must not be freed — the one exception to the ownership rule, and
        // freeing it would be heap corruption that only shows up later.
        guard let p = abiVersion() else { return "" }
        return String(cString: p)
    }

    /// Takes ownership of a `char*` error message, copies it, and **frees it**.
    /// Letting the message reach a Swift `Error` without this is the classic
    /// C-ABI binding leak: error strings are malloc'd exactly like results.
    func takeError(_ err: UnsafeMutablePointer<CChar>?) -> LLMuxError {
        guard let err else { return .llmux("llmux failed without a message") }
        let msg = String(cString: err)
        free(err)
        return .llmux(msg)
    }

    /// Takes ownership of a `char*` result, copies it, and frees it.
    func takeString(_ p: UnsafeMutablePointer<CChar>) -> String {
        let s = String(cString: p)
        free(p)
        return s
    }
}

// MARK: - Locating the library

public enum LibraryLocator {
    /// The platform's file name for the shared library.
    ///
    /// Windows appears here for completeness only: **no `llmux.dll` has ever
    /// been built.** Swift on Windows cannot use direct mode today.
    public static var fileName: String {
        #if os(Windows)
        return "llmux.dll"
        #elseif os(macOS)
        return "libllmux.dylib"
        #else
        return "libllmux.so"
        #endif
    }

    /// Finds `libllmux`, in this order:
    ///
    /// 1. `$LLMUX_LIBRARY`, if set — an explicit path always wins.
    /// 2. `dist/ffi/<goos>_<goarch>/`, walking up from the current directory,
    ///    which is where `scripts/build-ffi.sh` puts it in a checkout.
    /// 3. The bare file name, handed to the platform loader.
    public static func find() throws -> String {
        var tried: [String] = []

        if let explicit = ProcessInfo.processInfo.environment["LLMUX_LIBRARY"], !explicit.isEmpty {
            if FileManager.default.fileExists(atPath: explicit) { return explicit }
            tried.append(explicit)
        }

        #if os(macOS)
        let goos = "darwin"
        #elseif os(Windows)
        let goos = "windows"
        #else
        let goos = "linux"
        #endif
        #if arch(arm64)
        let goarch = "arm64"
        #else
        let goarch = "amd64"
        #endif
        let relative = "dist/ffi/\(goos)_\(goarch)/\(fileName)"

        var dir = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
        while true {
            let candidate = dir.appendingPathComponent(relative).path
            if FileManager.default.fileExists(atPath: candidate) { return candidate }
            tried.append(candidate)
            let parent = dir.deletingLastPathComponent()
            if parent.path == dir.path { break }
            dir = parent
        }

        // Let the loader try the bare name; if it resolves, use it.
        if let h = dlopen(fileName, RTLD_NOW | RTLD_LOCAL) {
            _ = h  // never dlclose: see Library
            return fileName
        }
        tried.append(fileName)
        throw LLMuxError.libraryNotFound(tried)
    }
}

// MARK: - Gateway

/// An llmux gateway running in this process.
///
/// **Closing is `deinit`.** The handle is released when the last reference goes
/// away — on the happy path, on a `throw`, and on an early `return`. Swift's
/// ARC is the RAII here; there is no `close()` to forget, and no `defer` needed
/// at the call site.
///
/// The ABI documents a handle as safe to use from several threads at once, so
/// this type is `Sendable`.
public final class Gateway: @unchecked Sendable {
    private let lib: Library
    private let handle: UInt64

    /// Loads the library (see ``LibraryLocator/find()``) and creates a gateway.
    ///
    /// `configJSON` is an llmux configuration document — the same JSON
    /// `llmux serve` reads from `llmux.json`. `nil` means built-in defaults
    /// plus the environment.
    ///
    /// Construction is **inert**: no goroutines start and, unless the config
    /// names a Postgres DSN, no sockets open.
    public convenience init(configJSON: String? = nil) throws {
        try self.init(libraryPath: try LibraryLocator.find(), configJSON: configJSON)
    }

    /// As above, against a library at a known path.
    public init(libraryPath: String, configJSON: String? = nil) throws {
        let lib = try Library.shared(at: libraryPath)
        self.lib = lib

        var err: UnsafeMutablePointer<CChar>?
        let h: UInt64 = withOptionalCString(configJSON) { cfg in
            lib.new(cfg, &err)
        }
        guard h != 0 else { throw lib.takeError(err) }
        self.handle = h
    }

    /// As above, refusing to continue if the loaded library is not the expected
    /// version. Worth doing at startup: a shared library resolves off a load
    /// path you may not control.
    public convenience init(expectedVersion: String, configJSON: String? = nil) throws {
        let path = try LibraryLocator.find()
        let lib = try Library.shared(at: path)
        let loaded = lib.version
        guard loaded == expectedVersion else {
            throw LLMuxError.versionMismatch(loaded: loaded, expected: expectedVersion)
        }
        try self.init(libraryPath: path, configJSON: configJSON)
    }

    deinit {
        // llmux_close is idempotent and aborts any stream still running on the
        // handle, so this is safe however we got here.
        lib.close(handle)
    }

    /// The version the loaded library was built from, e.g. `"0.1.2"`.
    public var abiVersion: String { lib.version }

    /// Aborts every call in flight on this gateway, without closing it: the
    /// handle stays open and the next call starts on a fresh context.
    ///
    /// `call`, `streamSync`, and therefore `chunks` all block the calling
    /// thread until they finish — this is the only way to abandon one from
    /// outside. Call it from another thread while a call is blocked and the
    /// blocked call returns `LLMuxError.llmux("context canceled")`; chunks
    /// already delivered stay delivered, and tokens already served are still
    /// metered.
    ///
    /// It is also safe to call from **inside a chunk callback** — unlike
    /// `llmux_close`, which must never be called from a callback because it
    /// waits for the very call that is running it, `llmux_cancel` does not
    /// wait. This is the only cancellation path available to a host whose
    /// callback is its only thread (Node, PHP), and it is exactly what
    /// ``chunks(_:)`` relies on being safe: cancelling one `AsyncThrowingStream`
    /// can race a chunk arriving on the background queue that is driving it.
    ///
    /// A no-op when nothing is running on this handle, and a no-op the second
    /// time, so a defensive `cancel()` in a cleanup path costs nothing.
    ///
    /// **This is per-HANDLE, not per-call.** It aborts every call in flight on
    /// this `Gateway`, including one your code did not mean to touch. There is
    /// no finer-grained cancel in the ABI — if you need to cancel one stream
    /// without disturbing another that happens to share a handle, the fix is
    /// one `Gateway` per cancellation scope. ``chunks(_:)`` calls this from
    /// `onTermination`, so cancelling or abandoning one streamed
    /// `AsyncSequence` aborts every other call in flight on the same gateway.
    public func cancel() {
        lib.cancel(handle)
    }

    /// One unary call. `method` is `"chat"`, `"embed"` or `"models"`; the
    /// request and the result are the same JSON the HTTP API uses.
    ///
    /// A `"chat"` request with `"stream": true` is **refused** rather than
    /// quietly served as one blob — use ``chunks(_:)``.
    public func call(_ method: String, _ requestJSON: String? = nil) throws -> String {
        var err: UnsafeMutablePointer<CChar>?
        let out: UnsafeMutablePointer<CChar>? = method.withCString { m in
            withOptionalCString(requestJSON) { req in
                lib.call(handle, m, req, &err)
            }
        }
        guard let out else { throw lib.takeError(err) }
        return lib.takeString(out)
    }

    /// `POST /v1/chat/completions`, non-streaming.
    public func chat(_ requestJSON: String) throws -> String {
        try call("chat", requestJSON)
    }

    /// `POST /v1/embeddings`.
    public func embed(_ requestJSON: String) throws -> String {
        try call("embed", requestJSON)
    }

    /// `GET /v1/models`, answered from memory with no upstream call.
    public func models() throws -> String {
        try call("models", nil)
    }

    // MARK: Streaming

    /// Streams a chat completion, calling `onChunk` once per chunk **on this
    /// thread**, and blocking until the stream ends.
    ///
    /// Return `true` from `onChunk` to keep streaming and `false` to stop.
    /// Stopping early is not an error — you returned `false`, so you already
    /// know — and this returns normally. Tokens already served are metered
    /// either way.
    ///
    /// The chunk string is owned by the library and valid only for the duration
    /// of the callback; this method copies it into a Swift `String` before
    /// handing it over, so the copy is yours.
    ///
    /// Prefer ``chunks(_:)`` unless you specifically want the blocking form.
    public func streamSync(_ requestJSON: String, onChunk: (String) -> Bool) throws {
        // A @convention(c) function cannot capture, so the closure travels
        // through user_data as an unmanaged pointer to a box.
        final class Box {
            let body: (String) -> Bool
            init(_ body: @escaping (String) -> Bool) { self.body = body }
        }
        // withoutActuallyEscaping: the closure does not outlive this call —
        // llmux_stream is documented to invoke it synchronously and to return
        // only once the stream has ended.
        try withoutActuallyEscaping(onChunk) { escaping in
            let box = Box(escaping)
            let user = Unmanaged.passUnretained(box).toOpaque()

            let trampoline: ChunkCb = { chunkPtr, userData in
                guard let userData else { return 1 }
                let box = Unmanaged<Box>.fromOpaque(userData).takeUnretainedValue()
                let text = chunkPtr.map { String(cString: $0) } ?? ""
                return box.body(text) ? 0 : 1
            }

            var err: UnsafeMutablePointer<CChar>?
            let rc: Int32 = requestJSON.withCString { req in
                "chat".withCString { m in
                    lib.stream(handle, m, req, trampoline, user, &err)
                }
            }
            // Keep `box` alive across the call; passUnretained alone does not.
            withExtendedLifetime(box) {}
            if rc != 0 { throw lib.takeError(err) }
        }
    }

    /// Streams a chat completion as an `AsyncSequence` of chunk JSON documents.
    ///
    /// ```swift
    /// for try await chunk in gw.chunks(requestJSON) {
    ///     print(chunk)
    /// }
    /// ```
    ///
    /// Each element is one `chat.completion.chunk` object — byte for byte what
    /// the HTTP API writes after `data: `, without the prefix and without the
    /// terminal `[DONE]` frame.
    ///
    /// # How this works, and what it does not give you
    ///
    /// `llmux_stream` is a **blocking call with a callback**; an `AsyncSequence`
    /// is a pull API. Bridging them needs somewhere for the blocking call to
    /// live, so this dispatches it to a background queue — *not* to the
    /// cooperative thread pool, because blocking one of those threads for the
    /// length of a model's answer is how a Swift concurrency program deadlocks.
    ///
    /// Chunks are yielded **as they arrive**, so time-to-first-token is real.
    /// But `AsyncThrowingStream.yield` never suspends — that is the whole
    /// difference between "buffered" and "backpressured" — so `.unbounded`
    /// buffering means a consumer slower than the model queues chunks in
    /// memory rather than pausing the producer. For a chat completion that is
    /// bounded by the answer length, and this SDK keeps `.unbounded`
    /// deliberately rather than switching to `.bufferingNewest`, which would
    /// silently drop earlier chunks — the wrong trade for text, where every
    /// chunk is content, not a replaceable sample. (The Rust binding for the
    /// same ABI uses a rendezvous channel and gets real backpressure — a
    /// difference in what the two languages' stream types offer, not in the
    /// ABI. The sidecar's stream, below, gets it from `URLSession.bytes(for:)`
    /// driving the socket read.)
    ///
    /// None of that touches the bug this method exists to not have: a
    /// consumer that stops reading must stop the *upstream provider*, not just
    /// stop queuing chunks nobody will read. `AsyncThrowingStream` calls
    /// `onTermination` with `.cancelled` exactly when that happens — the
    /// consuming `Task` is cancelled, or the iterator is simply abandoned by
    /// breaking out of the loop — and this method's `onTermination` reaches
    /// for ``cancel()`` there, not just a flag. A flag only helps once the
    /// *next* chunk arrives and the callback runs again; if the upstream has
    /// gone quiet between chunks, `streamSync` is blocked deep in a network
    /// read on the background queue with nothing scheduled to check a flag.
    /// `llmux_cancel` is the one thing that reaches into that blocked call
    /// from another thread and makes it return now, per fact 1 in the FFI
    /// contract: a cancelled `llmux_stream` returns promptly with
    /// `context canceled`, whatever it was doing.
    ///
    /// That C-ABI string is not what reaches the consumer, though: catching
    /// `LLMuxError.llmux("context canceled")` here and re-throwing
    /// `CancellationError()` is deliberate. `context canceled` is Go's
    /// `context.Canceled.Error()`, produced only when something cancels the
    /// call's context — llmux's own liveness timeouts return their own,
    /// differently worded errors (`stream_idle_timeout_seconds` and
    /// `stream_first_byte_timeout_seconds` in the configuration document), so
    /// this text is never ambiguous with "the model actually failed." A Swift
    /// consumer catching a cancelled `for try await` loop should see the
    /// idiomatic `CancellationError`, not a foreign-language error string it
    /// has no reason to pattern-match on.
    public func chunks(_ requestJSON: String) -> AsyncThrowingStream<String, Error> {
        AsyncThrowingStream(String.self, bufferingPolicy: .unbounded) { continuation in
            continuation.onTermination = { [self] reason in
                switch reason {
                case .cancelled:
                    // The consumer walked away — Task cancellation or a plain
                    // `break` — while `streamSync` below may still be blocked
                    // on the background queue. See ``cancel()`` for what this
                    // does and does not reach: it is per-HANDLE, so it also
                    // aborts any other call sharing this `Gateway`.
                    cancel()
                case .finished:
                    // The producer already returned, through completion or its
                    // own error. Nothing is left in flight to cancel, and
                    // calling `cancel()` anyway would be a no-op — see fact 4 —
                    // but there is no reason to reach for the ABI at all here.
                    break
                @unknown default:
                    cancel()
                }
            }

            DispatchQueue.global(qos: .userInitiated).async { [self] in
                do {
                    try streamSync(requestJSON) { chunk in
                        continuation.yield(chunk)
                        return true
                    }
                    continuation.finish()
                } catch LLMuxError.llmux("context canceled") {
                    continuation.finish(throwing: CancellationError())
                } catch {
                    continuation.finish(throwing: error)
                }
            }
        }
    }
}

/// Runs `body` with a C string for `s`, or with `nil` when `s` is `nil`.
///
/// `String.withCString` has no optional form, and the naive workaround —
/// `s?.withCString { … } ?? body(nil)` — does not type-check when `body`
/// throws or returns a non-Void value.
@inline(__always)
func withOptionalCString<R>(_ s: String?, _ body: (UnsafePointer<CChar>?) throws -> R) rethrows -> R {
    guard let s else { return try body(nil) }
    return try s.withCString { try body($0) }
}
