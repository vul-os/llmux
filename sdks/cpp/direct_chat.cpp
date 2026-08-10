// direct_chat.cpp — llmux in this process, through llmux.hpp.
//
//   make direct_chat && ./run-demo.sh direct_chat
//
// The C ABI is the real interface; llmux.hpp is a thin RAII wrapper over it, and
// this file is what using that wrapper looks like. Compare it with
// ../c/direct_chat.c: same calls, same order, same output — the difference is
// that nothing here needs a cleanup label, because every resource has an owner.
//
// Usage: direct_chat [config-json]
//        $LLMUX_CONFIG_JSON is used when no argument is given; with neither,
//        llmux uses built-in defaults plus providers auto-detected from the
//        environment.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <unistd.h>

#include <condition_variable>
#include <cstdio>
#include <cstdlib>
#include <iostream>
#include <mutex>
#include <string>
#include <string_view>
#include <thread>
#include <version>  // __cpp_lib_jthread: see llmux.hpp and the README for the Apple wart

#if defined(__cpp_lib_jthread)
#include <stop_token>
#endif

#include "llmux.hpp"

namespace {

const char *model() {
	const char *m = std::getenv("LLMUX_DEMO_MODEL");
	return (m && *m) ? m : "demo";
}

std::string chat_body(std::string_view prompt) {
	return std::string(R"({"model":")") + model() + R"(","messages":[{"role":"user","content":")" +
	       std::string(prompt) + R"("}]})";
}

// Pull the first "key":"value" out of a document, for printing. NOT a JSON
// parser — see ../c/jsonpeek.h for the same disclaimer at length. A real
// program links nlohmann/json or simdjson; llmux speaks ordinary OpenAI JSON.
std::string peek(std::string_view doc, std::string_view key) {
	const std::string needle = "\"" + std::string(key) + "\":\"";
	std::string out;
	size_t at = doc.find(needle);
	if (at == std::string_view::npos) return out;
	for (size_t i = at + needle.size(); i < doc.size() && doc[i] != '"'; i++) {
		if (doc[i] == '\\' && i + 1 < doc.size()) i++;
		out.push_back(doc[i]);
	}
	return out;
}

// Append every "content":"..." in the document. This is how a streamed answer
// is reassembled from its chunks.
void peek_append_all(std::string_view doc, std::string_view key, std::string &out) {
	const std::string needle = "\"" + std::string(key) + "\":\"";
	size_t at = 0;
	while ((at = doc.find(needle, at)) != std::string_view::npos) {
		at += needle.size();
		for (; at < doc.size() && doc[at] != '"'; at++) {
			if (doc[at] == '\\' && at + 1 < doc.size()) at++;
			out.push_back(doc[at]);
		}
	}
}

int count(std::string_view doc, std::string_view needle) {
	int n = 0;
	for (size_t at = doc.find(needle); at != std::string_view::npos;
	     at = doc.find(needle, at + needle.size()))
		n++;
	return n;
}

// Read the first "key": <number>. sdks/fake-upstream.py's JSON comes from
// Python's json.dumps, which puts a space after the colon by default, so this
// skips it -- the same thing jsonpeek.c's seek() does for ../c. Returns -1 if
// not found, which the one caller below treats as "could not measure."
double peek_number(std::string_view doc, std::string_view key) {
	const std::string needle = "\"" + std::string(key) + "\":";
	size_t at = doc.find(needle);
	if (at == std::string_view::npos) return -1;
	at += needle.size();
	while (at < doc.size() && doc[at] == ' ') at++;
	return std::strtod(std::string(doc.substr(at)).c_str(), nullptr);
}

// One GET against sdks/fake-upstream.py's /generated endpoint, e.g.
// {"generated": 3, "streams": 2, "disconnects": 2} -- outside the ABI
// entirely, so it has no business in llmux.hpp. Not a general HTTP client;
// see sidecar_chat.cpp for the fuller version this borrows its shape from.
// Returns -1 on any failure to reach it.
double fetch_generated(const char *upstream_url) {
	int port = 0;
	if (!upstream_url || std::sscanf(upstream_url, "http://127.0.0.1:%d", &port) != 1 || port <= 0)
		return -1;

	int fd = ::socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return -1;
	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_port = htons(static_cast<uint16_t>(port));
	::inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);
	if (::connect(fd, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) != 0) {
		::close(fd);
		return -1;
	}
	const std::string req = "GET /generated HTTP/1.1\r\nHost: 127.0.0.1:" + std::to_string(port) +
	                        "\r\nConnection: close\r\n\r\n";
	if (::send(fd, req.data(), req.size(), 0) < 0) {
		::close(fd);
		return -1;
	}
	std::string raw;
	char buf[4096];
	for (;;) {
		ssize_t n = ::recv(fd, buf, sizeof(buf), 0);
		if (n <= 0) break;
		raw.append(buf, static_cast<size_t>(n));
	}
	::close(fd);
	if (raw.find("\r\n\r\n") == std::string::npos) return -1;
	return peek_number(raw, "generated");
}

}  // namespace

int main(int argc, char **argv) {
	const char *config = (argc > 1) ? argv[1] : std::getenv("LLMUX_CONFIG_JSON");

	// 1. Probe the version before committing to the rest. Static string: not
	//    owned, not freed.
	std::cout << "abi version:  " << llmux::abi_version() << "\n";

	try {
		// 2. The gateway owns its handle for the rest of this scope, including
		//    every path out of it.
		llmux::Gateway gw(config);
		std::cout << "gateway:      handle " << gw.handle() << "\n\n";

		// --- models ---------------------------------------------------------
		{
			const std::string out = gw.models();
			std::cout << "models        " << count(out, "\"id\":\"") << " available, first is \""
			          << peek(out, "id") << "\"\n";
		}

		// --- one chat completion ---------------------------------------------
		{
			const std::string out = gw.chat(chat_body("say something short"));
			std::cout << "chat          \"" << peek(out, "content") << "\"\n";
			std::cout << "              routed to model \"" << peek(out, "model") << "\"\n";
		}

		// --- streaming --------------------------------------------------------
		// The callback runs on this thread, synchronously. Printed, not assumed.
		{
			const auto caller = std::this_thread::get_id();
			bool off_thread = false;
			int chunks = 0;
			std::string text;
			gw.stream(chat_body("go"), [&](std::string_view chunk) {
				chunks++;
				if (std::this_thread::get_id() != caller) off_thread = true;
				peek_append_all(chunk, "content", text);
				return true;  // keep going
			});
			std::cout << "stream        " << chunks << " chunks: \"" << text << "\"\n";
			std::cout << "              callback ran on "
			          << (off_thread ? "ANOTHER THREAD" : "the calling thread") << "\n";
		}

		// --- returning false stops the stream, and is not an error -------------
		{
			int chunks = 0;
			std::string text;
			gw.stream(chat_body("go"), [&](std::string_view chunk) {
				chunks++;
				peek_append_all(chunk, "content", text);
				return chunks < 2;  // false on the second chunk
			});
			std::cout << "stream        stopped after " << chunks << " chunks: \"" << text
			          << "\" (no exception thrown)\n";
		}

		// --- llmux_cancel: aborting a stream from outside the read loop --------
		//
		// "Returning false" above stops the CONSUMER's read loop, from inside a
		// chunk it already has -- and llmux.hpp still gets you a stopped upstream
		// connection out of it. cancel() is for the shape of problem that has no
		// chunk to return false from: another thread deciding to give up, or a
		// timeout, while this one is blocked in try_stream/stream with nothing
		// to hand back yet.
		//
		// `gw` above is wired to ffi/fakeupstream, which answers instantly and
		// counts nothing, so there is no window in it to cancel into. This block
		// needs sdks/fake-upstream.py's --chunk-delay-ms and its GET /generated
		// counter, which run-demo.sh starts as a SECOND fake upstream just for
		// this section and passes through LLMUX_CANCEL_CONFIG_JSON /
		// LLMUX_CANCEL_UPSTREAM_URL. Run this binary by hand with neither set and
		// the section says so and gets out of the way.
		{
			const char *cancel_config = std::getenv("LLMUX_CANCEL_CONFIG_JSON");
			const char *cancel_upstream = std::getenv("LLMUX_CANCEL_UPSTREAM_URL");
			if (!cancel_config || !*cancel_config) {
				std::cout << "cancel        skipped: needs LLMUX_CANCEL_CONFIG_JSON, which\n"
				             "              run-demo.sh sets from sdks/fake-upstream.py "
				             "--chunk-delay-ms 100\n";
			} else {
				llmux::Gateway cgw(cancel_config);  // a second, independent handle
				const std::string creq = chat_body("go");

				// -- the raw primitive: another thread reaches in while this one
				// blocks in try_stream. std::thread here is standing in for "some
				// other part of the program decided to give up" -- a cancel button,
				// a deadline timer, a request that hung up.
				std::mutex mu;
				std::condition_variable cv;
				int chunks = 0;
				std::string text;
				std::thread canceller([&] {
					std::unique_lock<std::mutex> lock(mu);
					cv.wait(lock, [&] { return chunks >= 3; });
					lock.unlock();
					cgw.cancel();  // per-HANDLE: aborts every call in flight on cgw
				});
				const llmux::VoidResult r = cgw.try_stream(creq.c_str(), [&](std::string_view chunk) {
					std::lock_guard<std::mutex> lock(mu);
					chunks++;
					peek_append_all(chunk, "content", text);
					if (chunks == 3) cv.notify_one();
					return true;
				});
				canceller.join();
				std::cout << "cancel        (cancel(), another thread)  ok=" << std::boolalpha
				          << r.ok() << " error=\"" << r.error() << "\"\n";
				std::cout << "                                          consumer saw " << chunks
				          << " chunks: \"" << text << "\"\n";
				const double generated_a = fetch_generated(cancel_upstream);
				if (generated_a >= 0) {
					std::cout << "                                          upstream GET "
					             "/generated: "
					          << generated_a << " of 12\n";
				}

				// The handle survives a cancel: still open, still usable.
				const llmux::StringResult m = cgw.try_models();
				std::cout << "cancel        handle " << cgw.handle()
				          << " answers \"models\" after cancel: "
				          << (m.ok() ? "yes" : m.error()) << "\n";

#if defined(__cpp_lib_jthread)
				// -- the C++20 idiom: a std::jthread's stop_token cancels the stream
				// cooperatively, through the stream(..., stop_token) overload. No
				// mutex of OURS reaches into llmux this time -- the std::stop_callback
				// llmux.hpp registers internally does that, and unregisters it the
				// instant try_stream returns. The mutex below only protects `chunks2`
				// between this thread and the watcher; it never touches cgw.
				int chunks2 = 0;
				std::string text2;
				std::mutex mu2;
				std::condition_variable cv2;
				std::stop_source stopper;
				std::jthread watcher([&](std::stop_token /* the jthread's own, unused */) {
					std::unique_lock<std::mutex> lock(mu2);
					cv2.wait(lock, [&] { return chunks2 >= 3; });
					lock.unlock();
					stopper.request_stop();
				});
				const llmux::VoidResult r2 = cgw.try_stream(
				    creq.c_str(),
				    [&](std::string_view chunk) {
					    std::lock_guard<std::mutex> lock(mu2);
					    chunks2++;
					    peek_append_all(chunk, "content", text2);
					    if (chunks2 == 3) cv2.notify_one();
					    return true;
				    },
				    stopper.get_token());
				std::cout << "cancel        (jthread + stop_token)      ok=" << std::boolalpha
				          << r2.ok() << " error=\"" << r2.error() << "\"\n";
				std::cout << "                                          consumer saw " << chunks2
				          << " chunks: \"" << text2 << "\"\n";
				const double generated_total = fetch_generated(cancel_upstream);
				if (generated_a >= 0 && generated_total >= 0) {
					std::cout << "                                          upstream GET "
					             "/generated: "
					          << (generated_total - generated_a) << " of 12\n";
				}
#else
				std::cout << "cancel        (jthread + stop_token)      SKIPPED: this toolchain\n"
				             "                                          does not define "
				             "__cpp_lib_jthread (see README)\n";
#endif
			}
		}

		// --- an exception thrown inside the callback ---------------------------
		// It must not unwind through the Go call frame. llmux.hpp catches it at
		// the C boundary, stops the stream, and rethrows here.
		{
			int chunks = 0;
			try {
				gw.stream(chat_body("go"), [&](std::string_view) -> bool {
					chunks++;
					throw std::runtime_error("the callback said no");
				});
				std::cout << "callback      NOT PROPAGATED — the exception was swallowed\n";
			} catch (const std::runtime_error &e) {
				std::cout << "callback      threw after " << chunks << " chunk: " << e.what()
				          << " (propagated, stream stopped)\n";
			}
		}

		// --- the error path -----------------------------------------------------
		// The message is plain UTF-8 text, not JSON. The library allocated it;
		// OwnedString freed it before this exception was constructed, so a throw
		// is not a leak.
		try {
			(void)gw.call("no-such-method", "{}");
			std::cout << "error         an unknown method returned a result\n";
		} catch (const llmux::Error &e) {
			std::cout << "error         " << e.what() << "\n";
		}

		// --- the same failure without exceptions --------------------------------
		{
			const llmux::StringResult r = gw.try_call("no-such-method", "{}");
			std::cout << "try_call      ok=" << std::boolalpha << r.ok() << " error=\"" << r.error()
			          << "\"\n";
		}

		// --- RAII on the throw path ---------------------------------------------
		// A gateway created inside a scope that throws is still closed. This is
		// the guarantee the C example spells with a goto label.
		std::uint64_t leaked = 0;
		try {
			llmux::Gateway inner(config);
			leaked = inner.handle();
			throw std::runtime_error("something went wrong mid-block");
		} catch (const std::runtime_error &) {
			llmux::Gateway probe = llmux::Gateway::try_open(config).take();  // keep the lib busy
			std::cout << "unwind        handle " << leaked
			          << " was closed by ~Gateway during unwinding\n";
		}
	} catch (const llmux::Error &e) {
		std::cerr << "llmux: " << e.what() << "\n";
		return 1;
	}

	// Out of scope: closed. Use-after-close is a clean error, because handles
	// are registry keys and are never reused.
	std::cout << "\nafter close   ";
	{
		llmux::Gateway closed = llmux::Gateway::try_open(config).take();
		const std::uint64_t h = closed.handle();
		closed.close();
		closed.close();  // idempotent
		const llmux::StringResult r = closed.try_models();
		std::cout << "handle " << h << ": " << (r.ok() ? "the closed handle answered!" : r.error())
		          << "\n";
	}
	return 0;
}
