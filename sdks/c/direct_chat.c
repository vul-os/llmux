/*
 * direct_chat.c — llmux running IN THIS PROCESS, through the C ABI.
 *
 *   make direct_chat && ./run-demo.sh direct_chat
 *
 * C is where this ABI is defined, so this file is the reference reading of it:
 * every other language's binding in sdks/ is doing what happens here, with more
 * ceremony. It links against libllmux at build time and #includes the stable
 * header, which is how a program with an installed library is actually written.
 *
 * NOT A TEST. ffi/ctest/smoke.c is the test — it dlopen()s the library,
 * resolves the six symbols by name, and asserts 32 checks including how many
 * checks ran. It exists to catch a missing //export or a drifted header, which
 * is a different job from showing someone how to call this. If you are changing
 * the ABI, that file is the one that must fail.
 *
 * Usage: direct_chat [config-json]
 *        $LLMUX_CONFIG_JSON is used when no argument is given; with neither,
 *        llmux uses built-in defaults plus providers auto-detected from the
 *        environment.
 *
 * SPDX-License-Identifier: MIT OR Apache-2.0
 */

#define _POSIX_C_SOURCE 200809L

#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "jsonpeek.h"
#include "llmux.h"

#define MODEL "demo"

static const char *model(void) {
	const char *m = getenv("LLMUX_DEMO_MODEL");
	return (m && *m) ? m : MODEL;
}

/* ------------------------------------------------------------------ */
/* Streaming                                                           */
/* ------------------------------------------------------------------ */

struct stream_state {
	int chunks;
	int stop_after;   /* 0 = run to the end */
	char text[4096];  /* the answer, reassembled from the chunks */
	pthread_t caller;
	int off_caller_thread;
};

/*
 * One chunk. chunk_json is one chat.completion.chunk — byte for byte what the
 * HTTP API writes after `data: ` — and it is OWNED BY THE LIBRARY and valid
 * only until this function returns. json_string_append_all copies out of it
 * immediately, which is the only correct thing to do with it.
 *
 * Returning non-zero stops the stream, and is not an error.
 */
static int on_chunk(const char *chunk_json, void *user_data) {
	struct stream_state *st = (struct stream_state *)user_data;
	st->chunks++;
	if (!pthread_equal(pthread_self(), st->caller)) st->off_caller_thread = 1;
	json_string_append_all(chunk_json, "content", st->text, sizeof(st->text));
	return (st->stop_after && st->chunks >= st->stop_after) ? 1 : 0;
}

/* ------------------------------------------------------------------ */

int main(int argc, char **argv) {
	const char *config = (argc > 1) ? argv[1] : getenv("LLMUX_CONFIG_JSON");
	char *err = NULL;
	char *out = NULL;
	uint64_t h = 0;
	int status = 1;
	char req[512];

	/*
	 * 1. Probe the version before anything else. A shared library is resolved
	 *    off a load path you may not control, and a stale libllmux earlier on
	 *    it otherwise misbehaves in ways that look like llmux bugs. The
	 *    returned string is static: do NOT free it.
	 */
	printf("abi version:  %s\n", llmux_abi_version());

	/*
	 * 2. Create the gateway. Inert: no goroutines, and no sockets unless the
	 *    configuration names a Postgres DSN. Nothing happens until you call.
	 */
	h = llmux_new(config, &err);
	if (h == 0) {
		fprintf(stderr, "llmux_new: %s\n", err ? err : "(no message)");
		llmux_free(err); /* the error string is ours to free, like any result */
		return 1;
	}
	printf("gateway:      handle %llu\n\n", (unsigned long long)h);

	/*
	 * From here every exit goes through `done:`. There is no RAII in C, so the
	 * discipline is one cleanup label and no early returns — that is what keeps
	 * the handle and the malloc'd strings from leaking on an error path.
	 */

	/* --- models: answered from memory --------------------------------- */
	out = llmux_call(h, "models", NULL, &err);
	if (!out) {
		fprintf(stderr, "models: %s\n", err ? err : "(no message)");
		goto done;
	}
	{
		int n = 0;
		for (const char *p = out; (p = strstr(p, "\"id\":\"")) != NULL; p += 6) n++;
		char first[128] = "";
		json_string(out, "id", first, sizeof(first));
		printf("models        %d available, first is \"%s\"\n", n, first);
	}
	llmux_free(out);
	out = NULL;

	/* --- one chat completion ------------------------------------------ */
	snprintf(req, sizeof(req),
	         "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"say something short\"}]}",
	         model());
	out = llmux_call(h, "chat", req, &err);
	if (!out) {
		fprintf(stderr, "chat: %s\n", err ? err : "(no message)");
		goto done;
	}
	{
		char content[1024] = "";
		char routed[128] = "";
		double tokens = 0;
		json_string(out, "content", content, sizeof(content));
		json_string(out, "model", routed, sizeof(routed));
		json_number(out, "total_tokens", &tokens);
		printf("chat          \"%s\"\n", content);
		printf("              routed to model \"%s\", %.0f tokens\n", routed, tokens);
	}
	llmux_free(out);
	out = NULL;

	/* --- streaming ------------------------------------------------------ */
	snprintf(req, sizeof(req),
	         "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"go\"}]}", model());
	{
		struct stream_state st;
		memset(&st, 0, sizeof(st));
		st.caller = pthread_self();
		if (llmux_stream(h, "chat", req, on_chunk, &st, &err) != 0) {
			fprintf(stderr, "stream: %s\n", err ? err : "(no message)");
			goto done;
		}
		printf("stream        %d chunks: \"%s\"\n", st.chunks, st.text);
		printf("              callback ran on %s\n",
		       st.off_caller_thread ? "ANOTHER THREAD" : "the calling thread");
	}

	/* --- stopping early is not an error --------------------------------- */
	{
		struct stream_state st;
		memset(&st, 0, sizeof(st));
		st.caller = pthread_self();
		st.stop_after = 2;
		if (llmux_stream(h, "chat", req, on_chunk, &st, &err) != 0) {
			fprintf(stderr, "stream (abort): %s\n", err ? err : "(no message)");
			goto done;
		}
		/* err is untouched by an abort: you returned non-zero, so you already
		 * know it happened, and llmux does not hand back an error for your own
		 * decision. Tokens already served are metered either way. */
		printf("stream        stopped after %d chunks: \"%s\" (rc 0, err %s)\n",
		       st.chunks, st.text, err ? "SET" : "NULL");
	}

	/* --- the error path -------------------------------------------------- */
	out = llmux_call(h, "no-such-method", "{}", &err);
	if (out) {
		fprintf(stderr, "an unknown method returned a result\n");
		goto done;
	}
	/* The message is plain UTF-8 text, NOT JSON. Print it; do not parse it.
	 * And free it: an error string is allocated exactly like a result. */
	printf("error         %s\n", err ? err : "(no message)");
	llmux_free(err);
	err = NULL;

	/* --- an invented handle is an error, not a segfault ------------------ */
	out = llmux_call(999999, "models", NULL, &err);
	printf("bad handle    %s\n", err ? err : "(no message)");
	llmux_free(err);
	err = NULL;

	status = 0;

done:
	llmux_free(out);          /* llmux_free(NULL) is safe, so no branch here */
	llmux_free(err);
	llmux_close(h);           /* idempotent, and it aborts any running stream */
	llmux_close(h);           /* proving that, since cleanup paths repeat */

	out = llmux_call(h, "models", NULL, &err);
	printf("\nafter close   %s\n", err ? err : "(the closed handle answered!)");
	llmux_free(out);
	llmux_free(err);
	return status;
}
