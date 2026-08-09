// sidecar_chat.cpp — llmux as a child process, spoken to over loopback HTTP.
//
//   make sidecar_chat && ./run-demo.sh sidecar_chat
//
// No library is loaded here, and that is the point: this program forks, and a
// process that has loaded the Go runtime must not. direct_chat.cpp and this one
// are deliberately separate binaries.
//
// Everything with a lifetime gets a destructor — the socket, the child process,
// the response buffer — so there is no cleanup path to forget, including when
// an exception leaves the middle of a request.
//
// WHEN TO PREFER THIS OVER direct_chat.cpp:
//   - your process forks (the Go runtime does not survive fork without exec)
//   - your process installs its own SIGSEGV/SIGBUS/SIGPROF handling, or you
//     build with sanitizers or ship a crash reporter
//   - you need per-tenant virtual keys, budgets or model allow-lists, which are
//     enforced by the HTTP auth middleware and absent from the C ABI by design
//   - there is no prebuilt library for your platform (see README.md)
//
// The socket code here is just enough HTTP/1.1 for 127.0.0.1 with
// `Connection: close`. It is not an HTTP client: no TLS, no chunked encoding,
// no keep-alive, no redirects. A real program links libcurl or cpp-httplib.
//
// SPDX-License-Identifier: MIT OR Apache-2.0

#include <arpa/inet.h>
#include <netinet/in.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <iostream>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>

namespace {

const char *model() {
	const char *m = std::getenv("LLMUX_DEMO_MODEL");
	return (m && *m) ? m : "demo";
}

const char *binary_path() {
	const char *b = std::getenv("LLMUX_BINARY");
	return (b && *b) ? b : "llmux";  // resolved on PATH by execvp
}

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

// ---------------------------------------------------------------------------
// Socket — a file descriptor with a destructor.
// ---------------------------------------------------------------------------

class Socket {
public:
	explicit Socket(int port) {
		fd_ = ::socket(AF_INET, SOCK_STREAM, 0);
		if (fd_ < 0) throw std::runtime_error("socket: " + std::string(std::strerror(errno)));
		sockaddr_in addr{};
		addr.sin_family = AF_INET;
		addr.sin_port = htons(static_cast<uint16_t>(port));
		::inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);
		if (::connect(fd_, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) != 0) {
			const std::string why = std::strerror(errno);
			::close(fd_);
			fd_ = -1;
			throw std::runtime_error("connect: " + why);
		}
	}
	~Socket() {
		if (fd_ >= 0) ::close(fd_);
	}
	Socket(const Socket &) = delete;
	Socket &operator=(const Socket &) = delete;

	void send_all(std::string_view data) const {
		while (!data.empty()) {
			ssize_t n = ::send(fd_, data.data(), data.size(), 0);
			if (n <= 0) {
				if (n < 0 && errno == EINTR) continue;
				throw std::runtime_error("send: " + std::string(std::strerror(errno)));
			}
			data.remove_prefix(static_cast<size_t>(n));
		}
	}

	/// Read until EOF, or until `sink` returns false.
	void read_all(const std::function<bool(std::string_view)> &sink) const {
		char buf[4096];
		for (;;) {
			ssize_t n = ::recv(fd_, buf, sizeof(buf), 0);
			if (n < 0) {
				if (errno == EINTR) continue;
				throw std::runtime_error("recv: " + std::string(std::strerror(errno)));
			}
			if (n == 0) return;
			if (!sink(std::string_view(buf, static_cast<size_t>(n)))) return;
		}
	}

private:
	int fd_ = -1;
};

std::string request_head(int port, std::string_view method, std::string_view path,
                         std::string_view accept, size_t body_len) {
	return std::string(method) + " " + std::string(path) + " HTTP/1.1\r\n" +
	       "Host: 127.0.0.1:" + std::to_string(port) + "\r\n" +
	       "User-Agent: llmux-cpp-example/1\r\n" + "Accept: " + std::string(accept) + "\r\n" +
	       "Authorization: Bearer llmux-local\r\n" + "Content-Type: application/json\r\n" +
	       "Content-Length: " + std::to_string(body_len) + "\r\n" + "Connection: close\r\n\r\n";
}

struct Response {
	int status = 0;
	std::string body;
};

Response http_request(int port, std::string_view method, std::string_view path,
                      std::string_view body) {
	Socket sock(port);
	sock.send_all(request_head(port, method, path, "application/json", body.size()));
	if (!body.empty()) sock.send_all(body);

	std::string raw;
	sock.read_all([&](std::string_view chunk) {
		raw.append(chunk);
		return true;
	});

	Response r;
	const size_t sep = raw.find("\r\n\r\n");
	if (sep == std::string::npos) throw std::runtime_error("no header terminator in the response");
	std::sscanf(raw.c_str(), "HTTP/%*d.%*d %d", &r.status);
	r.body = raw.substr(sep + 4);
	return r;
}

/// POST and hand over each `data:` line as it arrives. Buffering the whole
/// response first would make time-to-first-token a lie.
void http_sse(int port, std::string_view path, std::string_view body,
              const std::function<bool(std::string_view)> &on_event) {
	Socket sock(port);
	sock.send_all(request_head(port, "POST", path, "text/event-stream", body.size()));
	sock.send_all(body);

	std::string pending;
	bool past_headers = false;
	sock.read_all([&](std::string_view chunk) {
		pending.append(chunk);
		if (!past_headers) {
			const size_t sep = pending.find("\r\n\r\n");
			if (sep == std::string::npos) return true;
			pending.erase(0, sep + 4);
			past_headers = true;
		}
		size_t nl;
		while ((nl = pending.find('\n')) != std::string::npos) {
			std::string line = pending.substr(0, nl);
			pending.erase(0, nl + 1);
			while (!line.empty() && line.back() == '\r') line.pop_back();
			if (line.rfind("data: ", 0) == 0 && !on_event(std::string_view(line).substr(6)))
				return false;
		}
		return true;
	});
}

int free_port() {
	int fd = ::socket(AF_INET, SOCK_STREAM, 0);
	if (fd < 0) return 0;
	sockaddr_in addr{};
	addr.sin_family = AF_INET;
	addr.sin_port = 0;
	::inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);
	int port = 0;
	socklen_t len = sizeof(addr);
	if (::bind(fd, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) == 0 &&
	    ::getsockname(fd, reinterpret_cast<sockaddr *>(&addr), &len) == 0) {
		port = ntohs(addr.sin_port);
	}
	::close(fd);
	return port;  // racy by construction; the health poll catches a lost race
}

// ---------------------------------------------------------------------------
// Sidecar — a child process with a destructor.
// ---------------------------------------------------------------------------

class Sidecar {
public:
	Sidecar() {
		port_ = free_port();
		if (port_ == 0) throw std::runtime_error("no free loopback port");

		pid_ = ::fork();
		if (pid_ < 0) throw std::runtime_error("fork: " + std::string(std::strerror(errno)));
		if (pid_ == 0) {
			// Child. The environment is inherited, so OPENAI_API_KEY and
			// friends pass through and providers are auto-detected exactly as
			// for the standalone binary.
			const std::string addr = "127.0.0.1:" + std::to_string(port_);
			::setenv("LLMUX_ADDR", addr.c_str(), 1);
			::execlp(binary_path(), binary_path(), nullptr);
			std::cerr << "exec " << binary_path() << ": " << std::strerror(errno) << "\n";
			::_exit(127);
		}

		if (!wait_healthy(std::chrono::seconds(10))) {
			stop();  // the destructor would too, but the throw should not leave a child running
			throw std::runtime_error("llmux did not become healthy on 127.0.0.1:" +
			                         std::to_string(port_));
		}
	}

	~Sidecar() { stop(); }
	Sidecar(const Sidecar &) = delete;
	Sidecar &operator=(const Sidecar &) = delete;

	[[nodiscard]] int port() const noexcept { return port_; }
	[[nodiscard]] pid_t pid() const noexcept { return pid_; }
	[[nodiscard]] std::string base_url() const { return "http://127.0.0.1:" + std::to_string(port_); }

	/// Idempotent and noexcept: it runs from the destructor, possibly while an
	/// exception is in flight.
	void stop() noexcept {
		if (pid_ <= 0) return;
		::kill(pid_, SIGTERM);
		for (int i = 0; i < 100; i++) {  // up to 5s for a clean shutdown
			if (::waitpid(pid_, nullptr, WNOHANG) == pid_) {
				pid_ = -1;
				return;
			}
			std::this_thread::sleep_for(std::chrono::milliseconds(50));
		}
		::kill(pid_, SIGKILL);
		::waitpid(pid_, nullptr, 0);
		pid_ = -1;
	}

private:
	bool wait_healthy(std::chrono::seconds budget) const {
		const auto deadline = std::chrono::steady_clock::now() + budget;
		while (std::chrono::steady_clock::now() < deadline) {
			try {
				if (http_request(port_, "GET", "/health", "").status == 200) return true;
			} catch (const std::exception &) {
				// not up yet
			}
			std::this_thread::sleep_for(std::chrono::milliseconds(50));
		}
		return false;
	}

	int port_ = 0;
	pid_t pid_ = -1;
};

}  // namespace

int main() {
	try {
		Sidecar sidecar;
		std::cout << "sidecar:      " << sidecar.base_url() << "  (pid " << sidecar.pid() << ")\n";
		std::cout << "openai url:   " << sidecar.base_url() << "/v1\n\n";

		// --- models ----------------------------------------------------------
		{
			const Response r = http_request(sidecar.port(), "GET", "/v1/models", "");
			std::cout << "models        HTTP " << r.status << ", " << count(r.body, "\"id\":\"")
			          << " available, first is \"" << peek(r.body, "id") << "\"\n";
		}

		// --- one chat completion ------------------------------------------------
		const std::string body = std::string(R"({"model":")") + model() +
		                         R"(","messages":[{"role":"user","content":"say something short"}]})";
		{
			const Response r = http_request(sidecar.port(), "POST", "/v1/chat/completions", body);
			std::cout << "chat          HTTP " << r.status << ", \"" << peek(r.body, "content")
			          << "\"\n";
			std::cout << "              routed to model \"" << peek(r.body, "model") << "\"\n";
		}

		// --- streaming, as server-sent events -------------------------------------
		// Same JSON as direct mode plus "stream": true. No callback into a
		// foreign runtime and no thread question: the frames arrive on a socket
		// this process already reads.
		{
			const std::string stream_body =
			    std::string(R"({"model":")") + model() +
			    R"(","messages":[{"role":"user","content":"go"}],"stream":true})";
			int frames = 0;
			std::string text;
			http_sse(sidecar.port(), "/v1/chat/completions", stream_body,
			         [&](std::string_view data) {
				         if (data == "[DONE]") return false;
				         frames++;
				         peek_append_all(data, "content", text);
				         return true;
			         });
			std::cout << "stream        " << frames << " SSE frames: \"" << text << "\"\n";
		}

		// --- the error path ---------------------------------------------------------
		// The same wire contract, a different shape of failure: an HTTP status
		// and a JSON error body, rather than a char** err carrying plain text.
		{
			const Response r = http_request(sidecar.port(), "POST", "/v1/chat/completions",
			                                R"({"model":"no-such-model","messages":[]})");
			std::cout << "error         HTTP " << r.status << ": " << peek(r.body, "message")
			          << "\n";
		}

		// --- RAII on the throw path -----------------------------------------------
		// A sidecar whose scope exits by exception is still terminated.
		try {
			Sidecar temporary;
			std::cout << "unwind        started a second sidecar on port " << temporary.port()
			          << ", now throwing\n";
			throw std::runtime_error("something went wrong mid-block");
		} catch (const std::runtime_error &e) {
			std::cout << "unwind        ~Sidecar terminated it during unwinding (" << e.what()
			          << ")\n";
		}
	} catch (const std::exception &e) {
		std::cerr << "sidecar_chat: " << e.what() << "\n";
		return 1;
	}
	std::cout << "\nsidecar stopped\n";
	return 0;
}
