// llmux.hpp — an idiomatic C++ wrapper over the llmux C ABI.
//
// Header-only, C++17, no dependencies beyond the standard library and
// <llmux.h>. Link against libllmux (or dlopen it yourself and assign the
// function pointers — the C header declares the types for that).
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
//                     value with .ok(), .value and .error. Never throws.
//
// The throwing calls are one line each on top of the try_ ones. Define
// LLMUX_NO_EXCEPTIONS before including this header (or build with
// -fno-exceptions) to compile only the try_ layer.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

#ifndef LLMUX_HPP
#define LLMUX_HPP

#include <cstdint>
#include <functional>
#include <string>
#include <string_view>
#include <utility>

#include "llmux.h"

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

/// An expected-like pair. `error` is the library's message: plain UTF-8 text,
/// not JSON. Do not parse it.
template <typename T> struct Result {
	T value{};
	std::string error{};

	[[nodiscard]] bool ok() const noexcept { return error.empty(); }
	[[nodiscard]] explicit operator bool() const noexcept { return ok(); }

	static Result success(T v) { return Result{std::move(v), {}}; }
	static Result failure(std::string message) {
		return Result{T{}, message.empty() ? std::string("llmux failed without a message")
		                                   : std::move(message)};
	}
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
	if (!r.ok()) throw Error(r.error);
	return std::move(r.value);
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

private:
	explicit Gateway(std::uint64_t h) noexcept : h_(h) {}

	std::uint64_t h_ = 0;
};

}  // namespace llmux

#endif  // LLMUX_HPP
