/*
 * sidecar_chat.c — llmux as a child process, spoken to over loopback HTTP.
 *
 *   make sidecar_chat && ./run-demo.sh sidecar_chat
 *
 * Nobody runs a server: this program picks a free port, spawns `llmux` with
 * LLMUX_ADDR pointing at it, waits for /health, uses it, and kills it. That is
 * the same contract every SDK in sdks/ implements, written out in C so it is
 * clear how little there is to it.
 *
 * WHEN TO PREFER THIS OVER direct_chat.c, from C specifically:
 *   - Your process forks. The Go runtime inside libllmux does not survive
 *     fork() without exec(); a sidecar is a different process and does not
 *     care how often you fork.
 *   - Your process has its own SIGSEGV/SIGBUS/SIGPROF handling — a crash
 *     reporter, a sampling profiler, a sanitizer build. Go installs handlers
 *     for those.
 *   - You want per-tenant virtual keys, budgets or model allow-lists. Those
 *     are enforced by the HTTP auth middleware and are deliberately absent
 *     from the C ABI.
 *   - You are on a platform with no prebuilt library (see README.md).
 * Otherwise direct_chat.c is simpler: no port, no child, no HTTP.
 *
 * NOTE ON fork(): this program forks, and that is safe here precisely because
 * it never loads libllmux. Do not merge the two examples into one binary.
 *
 * SPDX-License-Identifier: MIT OR Apache-2.0
 */

#define _POSIX_C_SOURCE 200809L

#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "jsonpeek.h"
#include "mini_http.h"

extern char **environ;

static const char *model(void) {
	const char *m = getenv("LLMUX_DEMO_MODEL");
	return (m && *m) ? m : "demo";
}

static const char *binary(void) {
	const char *b = getenv("LLMUX_BINARY");
	return (b && *b) ? b : "llmux"; /* resolved on PATH by execvp */
}

/* ------------------------------------------------------------------ */
/* The sidecar's lifetime                                              */
/* ------------------------------------------------------------------ */

struct sidecar {
	pid_t pid;
	int port;
};

static void millisleep(long ms) {
	struct timespec ts = {ms / 1000, (ms % 1000) * 1000000L};
	nanosleep(&ts, NULL);
}

/* Poll /health until it answers 200 or the deadline passes. */
static int wait_healthy(int port, double seconds) {
	char errbuf[256];
	for (int i = 0; i < (int)(seconds * 20); i++) {
		http_response r;
		if (http_request(port, "GET", "/health", NULL, NULL, &r, errbuf, sizeof(errbuf)) == 0) {
			int ok = r.status == 200;
			http_response_free(&r);
			if (ok) return 0;
		}
		millisleep(50);
	}
	return -1;
}

static int sidecar_start(struct sidecar *s) {
	s->pid = -1;
	s->port = http_free_port();
	if (s->port == 0) {
		fprintf(stderr, "could not find a free loopback port\n");
		return -1;
	}

	char addr[64];
	snprintf(addr, sizeof(addr), "LLMUX_ADDR=127.0.0.1:%d", s->port);

	pid_t pid = fork();
	if (pid < 0) {
		perror("fork");
		return -1;
	}
	if (pid == 0) {
		/* Child. The environment is inherited, so OPENAI_API_KEY and friends
		 * pass through and the gateway auto-detects providers exactly as the
		 * standalone binary does. */
		putenv(addr);
		execvp(binary(), (char *const[]){(char *)binary(), NULL});
		fprintf(stderr, "exec %s: %s\n", binary(), strerror(errno));
		_exit(127);
	}
	s->pid = pid;

	if (wait_healthy(s->port, 10.0) != 0) {
		fprintf(stderr, "llmux did not become healthy on 127.0.0.1:%d\n", s->port);
		return -1;
	}
	return 0;
}

/* Idempotent, and called on every exit path including the error paths. */
static void sidecar_stop(struct sidecar *s) {
	if (!s || s->pid <= 0) return;
	kill(s->pid, SIGTERM);
	for (int i = 0; i < 100; i++) { /* up to 5s for a clean shutdown */
		int status = 0;
		pid_t got = waitpid(s->pid, &status, WNOHANG);
		if (got == s->pid) {
			s->pid = -1;
			return;
		}
		millisleep(50);
	}
	kill(s->pid, SIGKILL);
	waitpid(s->pid, NULL, 0);
	s->pid = -1;
}

/* ------------------------------------------------------------------ */
/* Streaming                                                           */
/* ------------------------------------------------------------------ */

struct sse_state {
	int frames;
	char text[4096];
};

static int on_event(const char *data, void *user_data) {
	struct sse_state *st = (struct sse_state *)user_data;
	if (strcmp(data, "[DONE]") == 0) return 1; /* stop reading */
	st->frames++;
	json_string_append_all(data, "content", st->text, sizeof(st->text));
	return 0;
}

/* ------------------------------------------------------------------ */

int main(void) {
	struct sidecar sc;
	char errbuf[256];
	char req[512];
	http_response r;
	int status = 1;

	if (sidecar_start(&sc) != 0) {
		sidecar_stop(&sc);
		return 1;
	}
	printf("sidecar:      http://127.0.0.1:%d  (pid %d)\n", sc.port, (int)sc.pid);
	printf("openai url:   http://127.0.0.1:%d/v1\n\n", sc.port);

	/* --- models ---------------------------------------------------------- */
	if (http_request(sc.port, "GET", "/v1/models", "llmux-local", NULL, &r, errbuf,
	                 sizeof(errbuf)) != 0) {
		fprintf(stderr, "GET /v1/models: %s\n", errbuf);
		goto done;
	}
	{
		int n = 0;
		for (const char *p = r.body; (p = strstr(p, "\"id\":\"")) != NULL; p += 6) n++;
		char first[128] = "";
		json_string(r.body, "id", first, sizeof(first));
		printf("models        HTTP %d, %d available, first is \"%s\"\n", r.status, n, first);
	}
	http_response_free(&r);

	/* --- one chat completion --------------------------------------------- */
	snprintf(req, sizeof(req),
	         "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"say something short\"}]}",
	         model());
	if (http_request(sc.port, "POST", "/v1/chat/completions", "llmux-local", req, &r, errbuf,
	                 sizeof(errbuf)) != 0) {
		fprintf(stderr, "POST /v1/chat/completions: %s\n", errbuf);
		goto done;
	}
	{
		char content[1024] = "";
		char routed[128] = "";
		double tokens = 0;
		json_string(r.body, "content", content, sizeof(content));
		json_string(r.body, "model", routed, sizeof(routed));
		json_number(r.body, "total_tokens", &tokens);
		printf("chat          HTTP %d, \"%s\"\n", r.status, content);
		printf("              routed to model \"%s\", %.0f tokens\n", routed, tokens);
	}
	http_response_free(&r);

	/* --- streaming, as server-sent events --------------------------------
	 * The body is the same JSON as direct mode's, plus "stream": true. No
	 * callback into a foreign runtime, no GIL, no thread-attach question — the
	 * frames arrive on a socket this process already reads. That is the
	 * sidecar's structural advantage for streaming in every language. */
	snprintf(req, sizeof(req),
	         "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"go\"}],\"stream\":true}",
	         model());
	{
		struct sse_state st;
		memset(&st, 0, sizeof(st));
		if (http_sse(sc.port, "/v1/chat/completions", "llmux-local", req, on_event, &st, errbuf,
		             sizeof(errbuf)) != 0) {
			fprintf(stderr, "SSE: %s\n", errbuf);
			goto done;
		}
		printf("stream        %d SSE frames: \"%s\"\n", st.frames, st.text);
	}

	/* --- the error path ---------------------------------------------------
	 * Same wire contract, different shape of failure: an HTTP status and a JSON
	 * error body, rather than a char** err carrying plain text. */
	snprintf(req, sizeof(req), "{\"model\":\"no-such-model\",\"messages\":[]}");
	if (http_request(sc.port, "POST", "/v1/chat/completions", "llmux-local", req, &r, errbuf,
	                 sizeof(errbuf)) != 0) {
		fprintf(stderr, "POST /v1/chat/completions: %s\n", errbuf);
		goto done;
	}
	{
		char message[512] = "";
		json_string(r.body, "message", message, sizeof(message));
		printf("error         HTTP %d: %s\n", r.status, message);
	}
	http_response_free(&r);

	status = 0;

done:
	sidecar_stop(&sc);
	printf("\nsidecar stopped\n");
	return status;
}
