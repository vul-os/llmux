/*
 * smoke.c — the C smoke test for libllmux.
 *
 * It dlopens the built shared library, resolves the seven exported symbols by
 * name, and drives one unary call and one streaming call end to end against a
 * fake OpenAI upstream. Without this, the ABI would be unverified: every Go
 * test in ffi/ passes just as well with a missing //export directive, a
 * misspelled symbol, or a header that disagrees with the library.
 *
 * Usage:
 *   smoke <library-path> <expected-version> <config-json> <expected-text>
 *
 * scripts/ffi-ctest.sh supplies all four: it builds the library, starts
 * ffi/fakeupstream (which prints the config JSON, so this test does not
 * hand-roll a second copy of it), and passes ../VERSION as the expected
 * version.
 *
 * The test ends by asserting the number of checks that actually ran. A C
 * program that returns 0 having executed nothing is the classic false green,
 * and dlsym failures are exactly the kind of thing an early `return 0` hides.
 */

#define _POSIX_C_SOURCE 200809L

#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "llmux.h"

/* Every check that must run, counted. See EXPECTED_CHECKS at the bottom. */
static int checks_run = 0;
static int failures = 0;

static void ok(const char *what) {
	checks_run++;
	printf("  ok   %s\n", what);
}

static void bad(const char *what, const char *detail) {
	checks_run++;
	failures++;
	printf("  FAIL %s: %s\n", what, detail ? detail : "(no detail)");
}

static void check(int cond, const char *what, const char *detail) {
	if (cond) {
		ok(what);
	} else {
		bad(what, detail);
	}
}

/* ------------------------------------------------------------------ */

static llmux_abi_version_fn p_abi_version;
static llmux_new_fn         p_new;
static llmux_close_fn       p_close;
static llmux_cancel_fn      p_cancel;
static llmux_call_fn        p_call;
static llmux_free_fn        p_free;
static llmux_stream_fn      p_stream;

/* Extract the value of the first "content":"..." in s, appending to out.
 * The fake upstream answers in plain ASCII words, so a naive scan that only
 * understands \" and \\ is enough — and is preferable to linking a JSON
 * parser into a test whose job is to check an ABI, not to parse JSON. */
static void append_content(const char *s, char *out, size_t outcap) {
	const char *needle = "\"content\":\"";
	const char *p = s;
	while ((p = strstr(p, needle)) != NULL) {
		p += strlen(needle);
		size_t len = strlen(out);
		while (*p && *p != '"') {
			char c = *p;
			if (c == '\\' && p[1]) {
				p++;
				c = *p;
				if (c == 'n') c = '\n';
			}
			if (len + 1 < outcap) {
				out[len++] = c;
				out[len] = '\0';
			}
			p++;
		}
	}
}

struct stream_state {
	int chunks;
	int stop_after;      /* 0 = never stop */
	char text[4096];
	pthread_t caller;
	int callback_off_caller_thread;
};

static int on_chunk(const char *chunk_json, void *user_data) {
	struct stream_state *st = (struct stream_state *)user_data;
	st->chunks++;
	if (!pthread_equal(pthread_self(), st->caller)) {
		st->callback_off_caller_thread = 1;
	}
	append_content(chunk_json, st->text, sizeof(st->text));
	if (st->stop_after && st->chunks >= st->stop_after) {
		return 1; /* ask llmux to stop */
	}
	return 0;
}

/* ------------------------------------------------------------------ */

int main(int argc, char **argv) {
	if (argc != 5) {
		fprintf(stderr, "usage: %s <library-path> <expected-version> <config-json> <expected-text>\n", argv[0]);
		return 2;
	}
	const char *libpath  = argv[1];
	const char *want_ver = argv[2];
	const char *config   = argv[3];
	const char *want_txt = argv[4];

	printf("llmux C smoke test\n  library: %s\n", libpath);

	void *lib = dlopen(libpath, RTLD_NOW | RTLD_LOCAL);
	if (!lib) {
		fprintf(stderr, "dlopen(%s) failed: %s\n", libpath, dlerror());
		return 1;
	}

	/* Resolve every symbol the header promises. A missing one is fatal: the
	 * remaining checks would crash rather than report. */
	struct { const char *name; void **slot; } syms[] = {
		{"llmux_abi_version", (void **)&p_abi_version},
		{"llmux_new",         (void **)&p_new},
		{"llmux_close",       (void **)&p_close},
		{"llmux_cancel",      (void **)&p_cancel},
		{"llmux_call",        (void **)&p_call},
		{"llmux_free",        (void **)&p_free},
		{"llmux_stream",      (void **)&p_stream},
	};
	for (size_t i = 0; i < sizeof(syms) / sizeof(syms[0]); i++) {
		dlerror();
		*syms[i].slot = dlsym(lib, syms[i].name);
		const char *e = dlerror();
		if (e || !*syms[i].slot) {
			fprintf(stderr, "dlsym(%s) failed: %s\n", syms[i].name, e ? e : "NULL");
			return 1;
		}
		ok(syms[i].name);
	}

	/* --- 1. version probe ------------------------------------------------ */
	const char *got_ver = p_abi_version();
	check(p_abi_version() == got_ver,
	      "llmux_abi_version returns the same pointer every call (a static, not a fresh malloc)",
	      "two calls returned different pointers — the header says do not free it, so a "
	      "per-call allocation would leak on every probe");
	if (got_ver && strcmp(got_ver, want_ver) == 0) {
		ok("llmux_abi_version matches the package version");
	} else {
		char detail[256];
		snprintf(detail, sizeof(detail), "library says %s, this tree is %s — a stale library is on the load path",
		         got_ver ? got_ver : "(null)", want_ver);
		bad("llmux_abi_version matches the package version", detail);
	}

	/* --- 2. a bad config is refused, with a message ---------------------- */
	{
		char *err = NULL;
		uint64_t h = p_new("{\"providers\": [", &err);
		check(h == 0, "llmux_new refuses a malformed configuration", "it returned a handle");
		check(err != NULL, "llmux_new sets *err on failure", "err was left NULL");
		if (err) p_free(err);
	}

	/* --- 3. open a real gateway ------------------------------------------ */
	char *err = NULL;
	uint64_t h = p_new(config, &err);
	if (h == 0) {
		fprintf(stderr, "llmux_new failed: %s\n", err ? err : "(no error message)");
		if (err) p_free(err);
		return 1;
	}
	ok("llmux_new returned a handle");

	/* --- 4. models -------------------------------------------------------- */
	{
		err = NULL;
		char *out = p_call(h, "models", NULL, &err);
		check(out != NULL, "llmux_call models returned a result", err ? err : "NULL with no error");
		if (out) {
			check(strstr(out, "\"object\":\"list\"") != NULL,
			      "models result is an OpenAI model list", out);
			p_free(out);
		}
		if (err) p_free(err);
	}

	/* --- 5. one unary chat ------------------------------------------------ */
	{
		err = NULL;
		const char *req = "{\"model\":\"demo\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}";
		char *out = p_call(h, "chat", req, &err);
		check(out != NULL, "llmux_call chat returned a result", err ? err : "NULL with no error");
		if (out) {
			check(strstr(out, "\"chat.completion\"") != NULL,
			      "chat result is a chat.completion object", out);
			check(strstr(out, want_txt) != NULL,
			      "chat result carries the upstream's answer", out);
			check(strstr(out, "\"upstream-model\"") != NULL,
			      "chat result names the ROUTED target model (routing ran)", out);
			p_free(out);
		}
		if (err) p_free(err);
	}

	/* --- 6a. a successful call RESETS *err -------------------------------- */
	/* A binding declares one `char *err = NULL;` and reuses it. If a success
	 * leaves the previous failure's message in place, the binding frees it a
	 * second time later and the double free lands in the host's allocator. */
	{
		char *reused = NULL;
		char *bad = p_call(h, "no-such-method", "{}", &reused);
		check(bad == NULL && reused != NULL, "the setup call for the *err reset check failed",
		      "it did not produce an error to leave behind");
		char *stale = reused;
		char *good = p_call(h, "models", NULL, &reused);
		check(good != NULL, "the reused-err probe's second call succeeded", "it failed");
		check(reused == NULL,
		      "a successful llmux_call resets *err, so a reused err cannot be double-freed",
		      "the earlier failure's message survived a successful call");
		if (good) p_free(good);
		p_free(stale);

		/* Same for llmux_new. */
		char *nerr = NULL;
		uint64_t bogus = p_new("{\"providers\": [", &nerr);
		check(bogus == 0 && nerr != NULL, "the setup call for llmux_new's *err reset check failed",
		      "it did not produce an error to leave behind");
		char *nstale = nerr;
		uint64_t h2 = p_new(config, &nerr);
		check(h2 != 0 && nerr == NULL, "a successful llmux_new resets *err",
		      nerr ? nerr : "llmux_new failed");
		if (h2) p_close(h2);
		p_free(nstale);
	}

	/* --- 6. errors are errors, not crashes -------------------------------- */
	{
		err = NULL;
		char *out = p_call(h, "no-such-method", "{}", &err);
		check(out == NULL, "llmux_call rejects an unknown method", "it returned a result");
		check(err != NULL && strstr(err, "unknown method") != NULL,
		      "unknown method sets a readable *err", err ? err : "(NULL)");
		if (err) p_free(err);

		err = NULL;
		out = p_call(999999, "models", NULL, &err);
		check(out == NULL, "llmux_call rejects an invented handle", "it returned a result");
		check(err != NULL && strstr(err, "unknown handle") != NULL,
		      "an invented handle is a clean error, not a segfault", err ? err : "(NULL)");
		if (err) p_free(err);
	}

	/* --- 7. one streaming chat -------------------------------------------- */
	{
		struct stream_state st;
		memset(&st, 0, sizeof(st));
		st.caller = pthread_self();
		err = NULL;
		const char *req = "{\"model\":\"demo\",\"messages\":[{\"role\":\"user\",\"content\":\"go\"}]}";
		int rc = p_stream(h, "chat", req, on_chunk, &st, &err);
		check(rc == 0, "llmux_stream returned 0", err ? err : "non-zero with no error");
		check(st.chunks >= 4, "the stream delivered several chunks (it was streamed, not batched)", "too few chunks");
		check(strcmp(st.text, want_txt) == 0,
		      "the chunks reassemble into the upstream's answer", st.text);
		check(st.callback_off_caller_thread == 0,
		      "the chunk callback ran on the thread that called llmux_stream",
		      "it ran on another thread — ffi/README.md and llmux.h claim otherwise");
		if (err) p_free(err);
	}

	/* --- 8. a callback that says stop, stops — and is not an error -------- */
	{
		struct stream_state st;
		memset(&st, 0, sizeof(st));
		st.caller = pthread_self();
		st.stop_after = 2;
		err = NULL;
		const char *req = "{\"model\":\"demo\",\"messages\":[{\"role\":\"user\",\"content\":\"go\"}]}";
		int rc = p_stream(h, "chat", req, on_chunk, &st, &err);
		check(rc == 0, "aborting from the callback is not reported as a failure",
		      err ? err : "llmux_stream returned non-zero");
		check(err == NULL, "an aborted stream leaves *err NULL", err ? err : "");
		check(st.chunks == 2, "the stream stopped when the callback asked", "it kept going");
		if (err) p_free(err);
	}

	/* --- 9. a NULL callback is refused ------------------------------------ */
	{
		err = NULL;
		int rc = p_stream(h, "chat", "{\"model\":\"demo\",\"messages\":[]}", NULL, NULL, &err);
		check(rc == -1, "llmux_stream refuses a NULL callback", "it returned success");
		check(err != NULL, "the NULL-callback refusal sets *err", "err was left NULL");
		if (err) p_free(err);
	}

	/* --- 10. cancel with nothing in flight is a no-op, and the handle lives -- */
	{
		p_cancel(h);
		p_cancel(999999); /* an invented handle must not be a crash either */
		err = NULL;
		char *out = p_call(h, "models", NULL, &err);
		check(out != NULL, "the handle still works after llmux_cancel",
		      err ? err : "NULL with no error — cancel closed the handle");
		if (out) p_free(out);
		if (err) p_free(err);
	}

	/* --- 11. close, twice -------------------------------------------------- */
	p_close(h);
	p_close(h); /* must be a no-op, not a double free */
	ok("llmux_close is idempotent");
	{
		err = NULL;
		char *out = p_call(h, "models", NULL, &err);
		check(out == NULL && err != NULL, "use-after-close is a clean error", "the closed handle answered");
		if (out) p_free(out);
		if (err) p_free(err);
	}

	/* llmux_free(NULL) must be safe: bindings call it on every path. */
	p_free(NULL);
	ok("llmux_free(NULL) is safe");

	/* --- the gate on this test itself -------------------------------------- */
	/* Update this when checks are added. It exists because a C test that
	 * exits 0 having run three of its thirty-two checks looks exactly like
	 * one that ran them all. */
	const int EXPECTED_CHECKS = 40;
	printf("\n%d checks ran, %d failed (expected %d checks)\n", checks_run, failures, EXPECTED_CHECKS);
	if (checks_run != EXPECTED_CHECKS) {
		printf("FAIL: the smoke test ran %d checks, not %d — it took an early exit "
		       "or a check was removed without updating EXPECTED_CHECKS\n", checks_run, EXPECTED_CHECKS);
		return 1;
	}
	if (failures) {
		printf("FAIL: %d check(s) failed\n", failures);
		return 1;
	}
	printf("PASS\n");
	return 0;
}
