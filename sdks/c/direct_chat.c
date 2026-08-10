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
 * resolves the seven symbols by name, and asserts 40 checks including how many
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
#include "mini_http.h"

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
/* Cancellation                                                        */
/* ------------------------------------------------------------------ */

/*
 * "Stopping early" above is the callback returning non-zero: the consumer's
 * own choice, discovered while reading a chunk it already has. llmux_cancel
 * is a different door: something OUTSIDE the read loop -- another thread, a
 * timeout, a request that hung up -- deciding to abandon a call that is
 * blocked in llmux_stream right now, with no chunk to return from.
 *
 * ffi/fakeupstream, which the gateway above is wired to, answers instantly
 * and keeps no count, so there is no window to cancel INTO and nothing to
 * check a cancellation against. sdks/fake-upstream.py exists for exactly
 * this: --chunk-delay-ms opens that window, and GET /generated reports what
 * the upstream actually wrote to its socket -- the only way to tell a
 * cancellation that truly stopped the provider from one that returned to the
 * caller promptly while generation, and metering, ran on regardless.
 * run-demo.sh starts a second fake upstream for this section and passes it
 * through LLMUX_CANCEL_CONFIG_JSON / LLMUX_CANCEL_UPSTREAM_URL. Run this
 * binary by hand with neither set and the section prints one line and gets
 * out of the way, rather than pretending to demonstrate something it can't.
 */

struct watch_state {
	pthread_mutex_t mu;
	pthread_cond_t cv;
	int chunks;
	char text[4096];
};

static void watch_state_init(struct watch_state *w) {
	memset(w, 0, sizeof(*w));
	pthread_mutex_init(&w->mu, NULL);
	pthread_cond_init(&w->cv, NULL);
}

static void watch_state_destroy(struct watch_state *w) {
	pthread_mutex_destroy(&w->mu);
	pthread_cond_destroy(&w->cv);
}

/*
 * Records chunks and wakes the canceller thread once three have arrived. It
 * does NOT call llmux_cancel itself -- that is the single-threaded demo,
 * below. This one is the multi-threaded idiom: a worker thread reaches into a
 * call that is blocked on a thread it does not share a stack with.
 */
static int on_chunk_watched(const char *chunk_json, void *user_data) {
	struct watch_state *w = (struct watch_state *)user_data;
	pthread_mutex_lock(&w->mu);
	w->chunks++;
	json_string_append_all(chunk_json, "content", w->text, sizeof(w->text));
	if (w->chunks == 3) pthread_cond_signal(&w->cv);
	pthread_mutex_unlock(&w->mu);
	return 0;
}

struct canceller_args {
	struct watch_state *w;
	uint64_t handle;
};

/*
 * Waits for the callback above to see three chunks, then calls llmux_cancel
 * from a thread doing nothing else -- the shape of a real cancel button, a
 * request-timeout timer, or a context whose deadline expired, none of which
 * live on the thread blocked inside llmux_stream. pthread_create is standing
 * in for "some other part of the program decided to give up."
 */
static void *canceller_thread(void *arg) {
	struct canceller_args *a = (struct canceller_args *)arg;
	pthread_mutex_lock(&a->w->mu);
	while (a->w->chunks < 3) pthread_cond_wait(&a->w->cv, &a->w->mu);
	pthread_mutex_unlock(&a->w->mu);
	llmux_cancel(a->handle); /* per-HANDLE: this aborts every call in flight on it */
	return NULL;
}

/*
 * The single-threaded route: the callback cancels the very call it is
 * running inside of. This is the one a host with no thread to spare for a
 * canceller -- Node's event loop, PHP-FPM, a fiber runtime -- has to use,
 * because the callback is the only code of theirs that runs while
 * llmux_stream blocks. VERIFIED SAFE, unlike llmux_close: close() waits (up
 * to 5s) for the very call running the callback and would deadlock here;
 * llmux_cancel does not wait, so calling it from inside the callback that is
 * about to be aborted is fine.
 */
struct self_cancel_state {
	int chunks;
	char text[4096];
	uint64_t handle;
};

static int on_chunk_self_cancel(const char *chunk_json, void *user_data) {
	struct self_cancel_state *s = (struct self_cancel_state *)user_data;
	s->chunks++;
	json_string_append_all(chunk_json, "content", s->text, sizeof(s->text));
	if (s->chunks == 3) llmux_cancel(s->handle);
	return 0;
}

/*
 * GET /generated from sdks/fake-upstream.py, e.g.
 * {"generated": 6, "streams": 2, "disconnects": 2}. Uses mini_http, which
 * this file otherwise has no reason to link -- one GET to a fake upstream is
 * exactly the "just enough HTTP/1.1 over loopback" it was written for.
 */
static int fetch_generated(const char *upstream_url, double *out) {
	int port = 0;
	if (!upstream_url || sscanf(upstream_url, "http://127.0.0.1:%d", &port) != 1 || port == 0)
		return -1;
	http_response r;
	char errbuf[256];
	if (http_request(port, "GET", "/generated", NULL, NULL, &r, errbuf, sizeof(errbuf)) != 0)
		return -1;
	int ok = json_number(r.body, "generated", out);
	http_response_free(&r);
	return ok ? 0 : -1;
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

	/* --- llmux_cancel: aborting a stream from outside the read loop ------- */
	{
		const char *cancel_config = getenv("LLMUX_CANCEL_CONFIG_JSON");
		const char *cancel_upstream = getenv("LLMUX_CANCEL_UPSTREAM_URL");
		if (!cancel_config || !*cancel_config) {
			printf("cancel        skipped: needs LLMUX_CANCEL_CONFIG_JSON, which run-demo.sh\n"
			       "              sets from sdks/fake-upstream.py --chunk-delay-ms 100\n");
		} else {
			char *cerr = NULL;
			uint64_t hc = llmux_new(cancel_config, &cerr);
			if (hc == 0) {
				fprintf(stderr, "cancel: llmux_new: %s\n", cerr ? cerr : "(no message)");
				llmux_free(cerr);
				goto done;
			}

			char creq[512];
			snprintf(creq, sizeof(creq),
			         "{\"model\":\"%s\",\"messages\":[{\"role\":\"user\",\"content\":\"go\"}]}",
			         model());

			/* -- from another thread, while this one blocks in llmux_stream -- */
			struct watch_state w;
			watch_state_init(&w);
			struct canceller_args ca;
			ca.w = &w;
			ca.handle = hc;
			pthread_t canceller;
			pthread_create(&canceller, NULL, canceller_thread, &ca);
			int rc = llmux_stream(hc, "chat", creq, on_chunk_watched, &w, &cerr);
			pthread_join(canceller, NULL);
			/* rc -1 / "context canceled" is the EXPECTED result of a cancellation,
			 * not a failure to report and abandon -- there is no goto done here. */
			printf("cancel        (another thread)   stream returned %d, err %s\n", rc,
			       cerr ? cerr : "NULL");
			printf("                                 consumer saw %d chunks: \"%s\"\n", w.chunks,
			       w.text);
			double generated_a = -1;
			int have_a = fetch_generated(cancel_upstream, &generated_a) == 0;
			if (have_a) {
				printf("                                 upstream GET /generated: %.0f of 12\n",
				       generated_a);
			}
			llmux_free(cerr);
			cerr = NULL;
			watch_state_destroy(&w);

			/* -- the handle survives: llmux_call after a cancel still works -- */
			out = llmux_call(hc, "models", NULL, &cerr);
			printf("cancel        handle %llu answers \"models\" after cancel: %s\n",
			       (unsigned long long)hc, out ? "yes" : (cerr ? cerr : "(no message)"));
			llmux_free(out);
			out = NULL;
			llmux_free(cerr);
			cerr = NULL;

			/* -- from inside the callback: the single-threaded route. A host with
			 * no thread to spare for a canceller -- Node's event loop, PHP-FPM, a
			 * fiber runtime -- has only this option, because the callback is the
			 * only code of theirs that runs while llmux_stream blocks. Verified
			 * safe: unlike llmux_close, which would deadlock waiting on the very
			 * call running this callback, llmux_cancel does not wait. */
			struct self_cancel_state sc;
			memset(&sc, 0, sizeof(sc));
			sc.handle = hc;
			rc = llmux_stream(hc, "chat", creq, on_chunk_self_cancel, &sc, &cerr);
			printf("cancel        (inside callback)  stream returned %d, err %s\n", rc,
			       cerr ? cerr : "NULL");
			printf("                                 consumer saw %d chunks: \"%s\"\n", sc.chunks,
			       sc.text);
			double generated_total = -1;
			if (have_a && fetch_generated(cancel_upstream, &generated_total) == 0) {
				printf("                                 upstream GET /generated: %.0f of 12\n",
				       generated_total - generated_a);
			}
			llmux_free(cerr);
			cerr = NULL;

			llmux_close(hc); /* a second handle, closed on its own -- h is unaffected */
		}
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
