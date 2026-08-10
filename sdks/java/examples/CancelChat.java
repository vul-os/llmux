import to.llmux.LlmuxDirect;
import to.llmux.LlmuxException;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Path;
import java.util.concurrent.CancellationException;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicInteger;

/**
 * llmux CANCELLATION from Java — llmux_cancel, and the two ways to reach it.
 *
 * DirectChat.java's "early stop" demo shows a handler returning {@code false}:
 * that stops the CONSUMER, but llmux and the provider keep going until the
 * next chunk arrives to notice — the callback simply is not invoked again.
 * This example is about the opposite problem: making the PROVIDER stop, which
 * needs {@code llmux_cancel} and is why this repo added it in v0.1.5.
 *
 * Run with sdks/java/run-examples.sh cancel, which starts
 * sdks/fake-upstream.py — the harness built for exactly this measurement,
 * because it is the only fake upstream here that both delays each chunk (so
 * there is time to cancel mid-stream) and counts, via GET /generated, how many
 * chunks it actually produced. ffi/fakeupstream (what the other examples use)
 * has neither.
 *
 * Two demonstrations, against the SAME running upstream, so /generated is read
 * as a running total and the delta across each one is what is reported:
 *
 *   1. LlmuxDirect.cancel(), called directly by a controller thread the
 *      instant it has seen 3 chunks. This is the tightest bound: no chunk has
 *      to arrive afterward for anything to notice.
 *   2. Future.cancel(true) on an ExecutorService task — the idiomatic Java
 *      construct. It does nothing but interrupt the thread running the task;
 *      LlmuxDirect.stream's trampoline is what turns that interrupt into
 *      llmux_cancel, and it can only check the interrupt flag once per chunk
 *      (see LlmuxDirect.stream's "Cancellation" section for why). So this
 *      path is one chunk-interval looser than calling cancel() directly, and
 *      the point of running both here is to measure that difference rather
 *      than assert it.
 *
 * Requires Java 22+ and --enable-native-access=ALL-UNNAMED, same as
 * DirectChat.java.
 */
public final class CancelChat {

    public static void main(String[] args) throws Exception {
        Path library = LlmuxDirect.findLibrary();
        System.out.println("library: " + library);

        String config = System.getenv("LLMUX_CONFIG_JSON");
        if (config == null || config.isEmpty()) {
            System.out.println();
            System.out.println("LLMUX_CONFIG_JSON is unset, so there is no upstream to cancel against.");
            System.out.println("Run this with sdks/java/run-examples.sh cancel.");
            return;
        }

        String generatedUrl = baseUrl(config) + "/generated";
        HttpClient http = HttpClient.newHttpClient();

        String streamRequest = "{"
                + "\"model\":\"demo\","
                + "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],"
                + "\"stream\":true"
                + "}";

        try (LlmuxDirect llmux = LlmuxDirect.open(library, config)) {
            int before = generatedCount(http, generatedUrl);

            int consumerDirect = runAndCancelDirectly(llmux, streamRequest);
            int afterDirect = generatedCount(http, generatedUrl);
            System.out.println("llmux.cancel() from another thread: consumer saw " + consumerDirect
                    + " chunk(s); upstream generated " + (afterDirect - before) + " for this run");

            int consumerFuture = runAndCancelViaFuture(llmux, streamRequest);
            int afterFuture = generatedCount(http, generatedUrl);
            System.out.println("Future.cancel(true): consumer saw " + consumerFuture
                    + " chunk(s); upstream generated " + (afterFuture - afterDirect) + " for this run");

            // Neither cancellation closed the handle: a plain unary call
            // still works, on the same LlmuxDirect both demos just cancelled.
            String models = llmux.call("models", null);
            System.out.println("handle survives both cancellations: models call returned "
                    + models.length() + " bytes");
        }

        System.out.println("done");
    }

    /**
     * Cancel from a second thread the instant the worker has seen 3 chunks,
     * by calling {@link LlmuxDirect#cancel()} directly — fact-checked against
     * ffi/include/llmux.h's "cancel from another thread while llmux_stream
     * blocks" case: the blocked call fails with "context canceled", which
     * this method treats as the expected outcome, not a bug.
     */
    private static int runAndCancelDirectly(LlmuxDirect llmux, String streamRequest) throws InterruptedException {
        AtomicInteger seen = new AtomicInteger();
        CountDownLatch threeChunks = new CountDownLatch(1);

        Thread worker = new Thread(() -> {
            try {
                llmux.stream("chat", streamRequest, chunk -> {
                    if (seen.incrementAndGet() == 3) {
                        threeChunks.countDown();
                    }
                    return true; // this handler never chooses to stop; cancel() does it from outside
                });
            } catch (LlmuxException expected) {
                // "llmux_stream(chat): context canceled" — cancel() below is
                // what causes this, so seeing it here is the demonstration
                // working, not a failure to report.
            } catch (InterruptedException e) {
                throw new AssertionError("nothing interrupts this thread in this demo", e);
            }
        }, "cancel-direct-demo");
        worker.start();

        threeChunks.await();
        llmux.cancel();
        worker.join();
        return seen.get();
    }

    /**
     * Cancel through {@code ExecutorService}/{@code Future} — what a Java
     * caller reaches for by habit, having never heard of {@code llmux_cancel}.
     * {@code Future.cancel(true)} only interrupts; {@link LlmuxDirect#stream}
     * is what turns that into {@code llmux_cancel}, and {@code Future.get()}
     * throwing {@link CancellationException} here is the {@code
     * java.util.concurrent} contract, not this binding's doing — it is
     * documented that way regardless of how the task's own thread responds to
     * the interrupt.
     */
    private static int runAndCancelViaFuture(LlmuxDirect llmux, String streamRequest) throws Exception {
        AtomicInteger seen = new AtomicInteger();
        CountDownLatch threeChunks = new CountDownLatch(1);
        ExecutorService pool = Executors.newSingleThreadExecutor();
        try {
            Future<Integer> future = pool.submit(() -> llmux.stream("chat", streamRequest, chunk -> {
                if (seen.incrementAndGet() == 3) {
                    threeChunks.countDown();
                }
                return true;
            }));

            threeChunks.await();
            boolean cancelled = future.cancel(true);
            if (!cancelled) {
                throw new AssertionError("future.cancel(true) reported the task as already finished");
            }
            try {
                int delivered = future.get();
                throw new AssertionError("stream ran to completion with " + delivered + " chunk(s) after cancel");
            } catch (CancellationException expected) {
                // See the method doc: this is Future's own contract firing —
                // and it fires the INSTANT cancel(true) is called, whether or
                // not the worker thread has noticed the interrupt yet. Taking
                // this as "the stream is done" and reading /generated right
                // now would be measuring a still-running race, not a result:
                // the worker thread can only observe Thread.interrupted() on
                // its NEXT chunk callback (see LlmuxDirect.stream's
                // "Cancellation" section), so it may still be blocked in
                // llmux_stream for up to one more chunk interval after
                // Future.get() has already thrown. awaitTermination below is
                // what actually waits for that worker thread to finish.
            }
            pool.shutdown();
            if (!pool.awaitTermination(10, java.util.concurrent.TimeUnit.SECONDS)) {
                throw new AssertionError("worker thread did not stop within 10s of being interrupted");
            }
            return seen.get();
        } finally {
            pool.shutdownNow();
        }
    }

    /** GET /generated from the fake upstream and pull out the "generated" count. */
    private static int generatedCount(HttpClient http, String url) throws Exception {
        HttpRequest req = HttpRequest.newBuilder(URI.create(url)).GET().build();
        String body = http.send(req, HttpResponse.BodyHandlers.ofString()).body();
        String key = "\"generated\"";
        int i = body.indexOf(key);
        if (i < 0) {
            throw new IllegalStateException("no \"generated\" field in " + body);
        }
        i = body.indexOf(':', i + key.length()) + 1;
        while (i < body.length() && Character.isWhitespace(body.charAt(i))) {
            i++;
        }
        int end = i;
        while (end < body.length() && Character.isDigit(body.charAt(end))) {
            end++;
        }
        return Integer.parseInt(body.substring(i, end));
    }

    /**
     * The fake upstream's config document names itself as
     * {@code "base_url": "http://127.0.0.1:PORT/v1"} — the OpenAI-style base a
     * provider entry always carries. Strip the {@code /v1} to get back the
     * plain HTTP server that also answers GET /generated.
     *
     * <p>Deliberately tolerant of whitespace around the colon: {@code
     * fake-upstream.py} pretty-prints with {@code json.dumps}'s default
     * separators (a space after {@code :}), while the Node/Deno/Bun harness's
     * hand-written JSON does not — a parser this small should not need to
     * care which.
     */
    private static String baseUrl(String configJson) {
        String key = "\"base_url\"";
        int i = configJson.indexOf(key);
        if (i < 0) {
            throw new IllegalStateException("no \"base_url\" in config json: " + configJson);
        }
        i = configJson.indexOf(':', i + key.length()) + 1;
        while (i < configJson.length() && Character.isWhitespace(configJson.charAt(i))) {
            i++;
        }
        if (i >= configJson.length() || configJson.charAt(i) != '"') {
            throw new IllegalStateException("\"base_url\" is not a JSON string in: " + configJson);
        }
        i++;
        int end = configJson.indexOf('"', i);
        String withV1 = configJson.substring(i, end);
        return withV1.endsWith("/v1") ? withV1.substring(0, withV1.length() - 3) : withV1;
    }
}
