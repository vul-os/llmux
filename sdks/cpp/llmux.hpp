// llmux.hpp — an idiomatic C++ wrapper over the llmux C ABI.
//
// Header-only, C++17, no dependencies beyond the standard library and
// <llmux.h>. Link against libllmux (or dlopen it yourself and assign the
// function pointers — the C header declares the types for that). Building as
// C++20 additionally unlocks a std::jthread/std::stop_token overload of
// stream() — see CANCELLATION below — but nothing about C++17 use changes.
//
//     #include "llmux.hpp"
//
//     llmux::Gateway gw;                       // env defaults; throws on failure
//     std::string answer = gw.chat(R"({"model":"gpt-4o-mini","messages":[...]})");
//     gw.stream(request, [](std::string_view chunk) {
//         std::cout << chunk;
//         return true;                         // false stops the stream
//     });
//                                              // ~Gateway closes the handle
//
// WHAT THIS WRAPPER IS FOR. The C ABI hands out two kinds of resource — a
// uint64 handle and malloc'd char* strings that only llmux_free may release —
// and both are easy to leak on an error path, which in C++ means any path,
// because any line can throw. So:
//
//   * Every char* the library returns is owned by llmux::OwnedString the
//     instant it comes back, INCLUDING the error message, and including the
//     message we are about to throw. Nothing between the call and the throw can
//     leak it, because there is no window in which it is a raw pointer.
//   * The handle is owned by llmux::Gateway, which is move-only and closes in
//     its destructor. The destructor is noexcept and llmux_close is idempotent,
//     so double-close and close-during-unwind are both fine.
//   * An exception thrown inside your stream callback is caught at the C
//     boundary, converted into "stop the stream", and rethrown once the C frame
//     has returned. Letting it unwind through a Go call frame would be
//     undefined behaviour.
//
// ERROR STYLE. Both are here and neither is a second implementation:
//
//   gw.chat(req)      throws llmux::Error, carrying the library's message.
//   gw.try_chat(req)  returns llmux::Result<std::string> — an expected-like
//                     value with .ok(), .value() and .error(). Never throws.
//
// The throwing calls are one line each on top of the try_ ones. Define
// LLMUX_NO_EXCEPTIONS before including this header (or build with
// -fno-exceptions) to compile only the try_ layer.
//
// CANCELLATION. llmux_cancel aborts every call in flight on a handle without
// closing it — the only way to get a thread out of a blocked llmux_stream
// short of destroying the whole gateway. Two ways to reach it:
//
//   gw.cancel()                                  // the raw primitive: fire and
//                                                 // forget, from any thread, or
//                                                 // from inside the chunk
//                                                 // callback itself
//
//   gw.stream(req, on_chunk, stop_token)          // C++20 only: register the
//                                                 // token from a std::jthread
//                                                 // (or any std::stop_source)
//                                                 // and letting it go out of
//                                                 // scope, or calling
//                                                 // request_stop(), cancels
//                                                 // the stream cooperatively
//
// Both are per-HANDLE, not per-stream: cancelling aborts every call in flight
// on that Gateway, including ones you were not thinking about. One Gateway per
// cancellation scope if that matters to you. See the README for the toolchain
// wart that gates the stop_token overload on Apple's libc++.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

#ifndef LLMUX_HPP
#define LLMUX_HPP

#include <cstdint>
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <utility>
#include <version>  // for __cpp_lib_jthread, the stop_token feature test macro

#include "llmux.h"

// std::jthread/std::stop_token is a C++20 library feature, not a language one,
// and some C++20-conforming toolchains still do not ship it — notably Apple's
// libc++ as of Xcode/CLT through at least clang 17, which requires
// -fexperimental-library to define this macro at all (see ../cpp/README.md).
// Feature-test rather than assume: building this header under an older
// standard, or a C++20 toolchain without the feature, degrades cleanly to
// cancel() and the existing try_stream()/stream() overloads instead of a
// compile error.
#if defined(__cpp_lib_jthread)
#include <stop_token>
#endif

#if !defined(LLMUX_NO_EXCEPTIONS) && !defined(__cpp_exceptions)
#define LLMUX_NO_EXCEPTIONS 1
#endif

#ifndef LLMUX_NO_EXCEPTIONS
#include <stdexcept>
#endif

namespace llmux {

// ---------------------------------------------------------------------------
// OwnedString — a char* the library allocated, released with llmux_free.
// ---------------------------------------------------------------------------

/// Move-only owner of a library-allocated string. Not std::unique_ptr with a
/// custom deleter, because that spells the ownership rule as a template
/// parameter; this spells it in the type's name, where a reader trips over it.
class OwnedString {
public:
	OwnedString() noexcept = default;
	explicit OwnedString(char *p) noexcept : p_(p) {}

	~OwnedString() { reset(); }

	OwnedString(OwnedString &&other) noexcept : p_(other.release()) {}
	OwnedString &operator=(OwnedString &&other) noexcept {
		if (this != &other) {
			reset();
			p_ = other.release();
		}
		return *this;
	}

	OwnedString(const OwnedString &) = delete;
	OwnedString &operator=(const OwnedString &) = delete;

	[[nodiscard]] const char *c_str() const noexcept { return p_; }
	[[nodiscard]] explicit operator bool() const noexcept { return p_ != nullptr; }

	/// A copy in std::string, so the library's allocation can go away.
	[[nodiscard]] std::string str() const { return p_ ? std::string(p_) : std::string(); }

	char *release() noexcept {
		char *p = p_;
		p_ = nullptr;
		return p;
	}

	void reset() noexcept {
		if (p_) {
			llmux_free(p_);  // llmux_free(nullptr) is safe; the branch is for clarity
			p_ = nullptr;
		}
	}

	/// Address to hand to a `char** err` out-parameter. Whatever the library
	/// writes there is owned from that moment on.
	char **err_slot() noexcept {
		reset();
		return &p_;
	}

private:
	char *p_ = nullptr;
};

// ---------------------------------------------------------------------------
// Result — the non-throwing return shape.
// ---------------------------------------------------------------------------

/// An expected-like value. `error()` is the library's message: plain UTF-8
/// text, not JSON. Do not parse it.
///
/// The value is held in a std::optional rather than as a plain member, and that
/// is not a style choice: a failure must not construct a T. With a plain member,
/// `Result<Gateway>::failure(...)` would default-construct a Gateway — opening a
/// real gateway on the failure path, from inside the code reporting that a
/// gateway could not be opened.
template <typename T> class Result {
public:
	[[nodiscard]] static Result success(T v) {
		Result r;
		r.value_.emplace(std::move(v));
		return r;
	}
	[[nodiscard]] static Result failure(std::string message) {
		Result r;
		r.error_ = message.empty() ? std::string("llmux failed without a message")
		                           : std::move(message);
		return r;
	}

	[[nodiscard]] bool ok() const noexcept { return error_.empty(); }
	[[nodiscard]] explicit operator bool() const noexcept { return ok(); }
	[[nodiscard]] const std::string &error() const noexcept { return error_; }

	/// Precondition: ok(). Reading it otherwise is a programming error.
	[[nodiscard]] const T &value() const & { return *value_; }
	[[nodiscard]] T take() { return std::move(*value_); }

private:
	std::optional<T> value_;
	std::string error_;
};

using StringResult = Result<std::string>;

struct Unit {};
using VoidResult = Result<Unit>;

#ifndef LLMUX_NO_EXCEPTIONS
/// Carries the library's message verbatim.
class Error : public std::runtime_error {
public:
	explicit Error(const std::string &message) : std::runtime_error(message) {}
};

namespace detail {
template <typename T> T unwrap(Result<T> r) {
	if (!r.ok()) throw Error(r.error());
	return r.take();
}
}  // namespace detail
#endif

// ---------------------------------------------------------------------------

/// The llmux version the loaded library was built from. Static storage inside
/// the library: never freed, hence a plain copy rather than an OwnedString.
///
/// Compare it at startup against the version you built against. A shared
/// library is resolved off a load path you may not control, and a stale
/// libllmux earlier on it misbehaves in ways that look like llmux bugs.
[[nodiscard]] inline std::string abi_version() {
	const char *v = llmux_abi_version();
	return v ? std::string(v) : std::string();
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

/// One llmux gateway, running in this process. Move-only; closes on destruction.
///
/// Construction is inert: no goroutines, and no sockets unless the
/// configuration names a Postgres DSN. A handle is safe to use from several
/// threads at once, and so is this object.
class Gateway {
public:
	/// Called once per chunk. Return false to stop the stream — which is a
	/// normal end, not a failure. `chunk` is one chat.completion.chunk and is
	/// valid only for the duration of the call; copy what you keep.
	using ChunkFn = std::function<bool(std::string_view chunk)>;

	/// Non-throwing construction. Check .ok() before using .value.
	[[nodiscard]] static Result<Gateway> try_open(const char *config_json = nullptr) {
		OwnedString err;
		std::uint64_t h = llmux_new(config_json, err.err_slot());
		if (h == 0) return Result<Gateway>::failure(err.str());
		return Result<Gateway>::success(Gateway(h));
	}

#ifndef LLMUX_NO_EXCEPTIONS
	/// Throws llmux::Error if the gateway cannot be created.
	explicit Gateway(const char *config_json = nullptr)
	    : Gateway(detail::unwrap(try_open(config_json))) {}

	explicit Gateway(const std::string &config_json) : Gateway(config_json.c_str()) {}
#endif

	~Gateway() { close(); }

	Gateway(Gateway &&other) noexcept : h_(other.h_) { other.h_ = 0; }
	Gateway &operator=(Gateway &&other) noexcept {
		if (this != &other) {
			close();
			h_ = other.h_;
			other.h_ = 0;
		}
		return *this;
	}

	Gateway(const Gateway &) = delete;
	Gateway &operator=(const Gateway &) = delete;

	[[nodiscard]] std::uint64_t handle() const noexcept { return h_; }
	[[nodiscard]] bool is_open() const noexcept { return h_ != 0; }

	/// Idempotent, and noexcept: it runs from the destructor, possibly during
	/// stack unwinding. Aborts any stream still running on the handle.
	void close() noexcept {
		if (h_ != 0) {
			llmux_close(h_);
			h_ = 0;
		}
	}

	/// Aborts every call in flight on this handle, WITHOUT closing it: the
	/// handle stays open and the next call starts on a fresh context. This is
	/// the only way to get another thread out of a blocked try_call/try_stream
	/// short of destroying the whole Gateway.
	///
	/// Safe from another thread while a call blocks, and safe from INSIDE the
	/// chunk callback of the very stream being cancelled — unlike close(),
	/// which must never be called from a callback because it waits (up to a
	/// few seconds) for the call running that callback to return. Calling
	/// cancel() on a closed handle, or one with nothing running, is a no-op,
	/// so it costs nothing to call defensively; noexcept for the same reason
	/// close() is.
	///
	/// PER-HANDLE, NOT PER-STREAM: this aborts every call in flight on THIS
	/// Gateway, not only the one you have in mind — the C ABI has no
	/// per-stream cancellation token. Two streams sharing a Gateway share this
	/// fate; give a stream its own Gateway if you need to cancel it alone.
	void cancel() noexcept {
		if (h_ != 0) llmux_cancel(h_);
	}

	// -- unary ------------------------------------------------------------

	[[nodiscard]] StringResult try_call(const char *method, const char *request_json) {
		if (h_ == 0) return StringResult::failure("gateway is closed");
		OwnedString err;
		OwnedString out(llmux_call(h_, method, request_json, err.err_slot()));
		if (!out) return StringResult::failure(err.str());
		return StringResult::success(out.str());
	}

	[[nodiscard]] StringResult try_chat(const char *request_json) {
		return try_call("chat", request_json);
	}
	[[nodiscard]] StringResult try_embed(const char *request_json) {
		return try_call("embed", request_json);
	}
	[[nodiscard]] StringResult try_models() { return try_call("models", nullptr); }

#ifndef LLMUX_NO_EXCEPTIONS
	std::string call(const char *method, const char *request_json) {
		return detail::unwrap(try_call(method, request_json));
	}
	/// Same body as POST /v1/chat/completions. "stream": true is refused here
	/// rather than quietly batched; use stream().
	std::string chat(const char *request_json) { return call("chat", request_json); }
	std::string chat(const std::string &request_json) { return chat(request_json.c_str()); }
	/// Same body as POST /v1/embeddings.
	std::string embed(const char *request_json) { return call("embed", request_json); }
	std::string embed(const std::string &request_json) { return embed(request_json.c_str()); }
	/// The OpenAI-shaped model list.
	std::string models() { return call("models", nullptr); }
#endif

	// -- streaming ---------------------------------------------------------

	/// Stream a chat completion, calling `on_chunk` once per chunk. Blocks
	/// until the stream ends.
	///
	/// The callback runs on THIS thread, synchronously, before this returns —
	/// measured with pthread_self() in ffi/ctest/smoke.c, not assumed. So there
	/// is no thread-registration dance here and none is written.
	[[nodiscard]] VoidResult try_stream(const char *request_json, const ChunkFn &on_chunk) {
		if (h_ == 0) return VoidResult::failure("gateway is closed");
		if (!on_chunk) return VoidResult::failure("no chunk callback");

		// The C callback cannot be a capturing lambda, so the state travels
		// through void* user_data. `escaped` is the whole reason this is not
		// three lines: an exception must not unwind through the Go call frame.
		struct State {
			const ChunkFn *fn;
#ifndef LLMUX_NO_EXCEPTIONS
			std::exception_ptr escaped;
#endif
		} state{&on_chunk
#ifndef LLMUX_NO_EXCEPTIONS
		        ,
		        nullptr
#endif
		};

		auto trampoline = [](const char *chunk_json, void *user_data) -> int {
			auto *st = static_cast<State *>(user_data);
#ifndef LLMUX_NO_EXCEPTIONS
			try {
				return (*st->fn)(std::string_view(chunk_json)) ? 0 : 1;
			} catch (...) {
				st->escaped = std::current_exception();
				return 1;  // stop the stream; rethrown below, outside the C frame
			}
#else
			return (*st->fn)(std::string_view(chunk_json)) ? 0 : 1;
#endif
		};

		OwnedString err;
		int rc = llmux_stream(h_, "chat", request_json, trampoline, &state, err.err_slot());
#ifndef LLMUX_NO_EXCEPTIONS
		if (state.escaped) std::rethrow_exception(state.escaped);
#endif
		if (rc != 0) return VoidResult::failure(err.str());
		return VoidResult::success(Unit{});
	}

#ifndef LLMUX_NO_EXCEPTIONS
	void stream(const char *request_json, const ChunkFn &on_chunk) {
		detail::unwrap(try_stream(request_json, on_chunk));
	}
	void stream(const std::string &request_json, const ChunkFn &on_chunk) {
		stream(request_json.c_str(), on_chunk);
	}
#endif

#if defined(__cpp_lib_jthread)
	// -- streaming with cooperative cancellation (C++20) -------------------

	/// Same as try_stream() above, plus a std::stop_token: a std::jthread
	/// calling request_stop() on its own token (which you pass here), the
	/// jthread simply being destroyed, or any std::stop_source's
	/// request_stop() reaches llmux_cancel through a std::stop_callback
	/// registered for exactly the duration of this call — which is the C++20
	/// idiom for "abandon this stream" and the reason it exists at all: without
	/// it, cancelling would mean reaching for cancel() and a synchronization
	/// primitive of your own, which is what this overload replaces.
	///
	/// The stop_callback is destroyed (and so unregistered) the moment
	/// llmux_stream returns, so a stop request arriving after the stream has
	/// already finished does nothing — the same no-op cancel() itself
	/// guarantees. If the token can never be stopped (stop.stop_possible() is
	/// false — e.g. a default-constructed std::stop_token bound to no source)
	/// this skips the registration and behaves exactly like the overload
	/// without a token.
	///
	/// PER-HANDLE, NOT PER-STREAM, same as cancel(): a stop request on this
	/// token aborts every call in flight on this Gateway, not only the one it
	/// was passed to. One Gateway per cancellation scope if that distinction
	/// matters.
	[[nodiscard]] VoidResult try_stream(const char *request_json, const ChunkFn &on_chunk,
	                                     std::stop_token stop) {
		if (!stop.stop_possible()) return try_stream(request_json, on_chunk);
		std::stop_callback on_stop(stop, [this] { cancel(); });
		return try_stream(request_json, on_chunk);
	}

#ifndef LLMUX_NO_EXCEPTIONS
	void stream(const char *request_json, const ChunkFn &on_chunk, std::stop_token stop) {
		detail::unwrap(try_stream(request_json, on_chunk, stop));
	}
	void stream(const std::string &request_json, const ChunkFn &on_chunk, std::stop_token stop) {
		stream(request_json.c_str(), on_chunk, stop);
	}
#endif
#endif  // __cpp_lib_jthread

private:
	explicit Gateway(std::uint64_t h) noexcept : h_(h) {}

	std::uint64_t h_ = 0;
};

}  // namespace llmux

#endif  // LLMUX_HPP
