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

#include <cstdlib>
#include <iostream>
#include <string>
#include <string_view>
#include <thread>

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
