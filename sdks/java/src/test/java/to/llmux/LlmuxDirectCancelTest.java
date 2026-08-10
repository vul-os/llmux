package to.llmux;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;

/**
 * Cancellation tests for {@link LlmuxDirect}: {@code llmux_cancel}, and the
 * two ways this binding reaches it — the low-level {@link LlmuxDirect#cancel}
 * and the idiomatic thread interruption documented on {@link
 * LlmuxDirect#stream}'s "Cancellation" section.
 *
 * <p>Gated on two external resources a unit JVM cannot fake: a built
 * {@code libllmux} (from {@code scripts/build-ffi.sh}, checked via {@link
 * LlmuxDirect#findLibrary()}) and {@code python3} on {@code PATH}, needed to
 * run {@code sdks/fake-upstream.py} — the only fake upstream in this repo that
 * both delays each chunk, so there is time to cancel mid-stream, and counts
 * via {@code GET /generated} what it actually produced. {@code
 * ffi/fakeupstream} (what {@link LlmuxSmokeTest}-adjacent tests would use) has
 * neither. Both are checked with {@code assumeTrue} rather than asserted, so a
 * checkout without them skips this class instead of failing it.
 */
class LlmuxDirectCancelTest {

    private static Process fakeUpstream;
    private static String configJson;
    private static String generatedUrl;
    private static LlmuxDirect llmux;
    private static final HttpClient HTTP = HttpClient.newHttpClient();

    @BeforeAll
    static void startFakeUpstreamAndGateway() throws Exception {
        Path library;
        try {
            library = LlmuxDirect.findLibrary();
        } catch (LlmuxException e) {
            fakeUpstream = null;
            assumeTrue(false, "no libllmux built: " + e.getMessage());
            return;
        }

        assumeTrue(onPath("python3"), "python3 is not on PATH");

        Path fakeUpstreamPy = findFakeUpstreamPy();
        assumeTrue(fakeUpstreamPy != null, "could not locate sdks/fake-upstream.py above " + here());

        // 30 ms/chunk: long enough that a second JVM thread can reliably
        // observe "3 chunks delivered" and act before the stream finishes on
        // its own, short enough that this test suite does not stall.
        ProcessBuilder pb = new ProcessBuilder("python3", fakeUpstreamPy.toString(),
                "--chunk-delay-ms", "30",
                "--text", "one two three four five six seven eight nine ten");
        pb.redirectErrorStream(true);
        fakeUpstream = pb.start();

        BufferedReader out = new BufferedReader(
                new InputStreamReader(fakeUpstream.getInputStream(), StandardCharsets.UTF_8));
        String url = null;
        for (int i = 0; i < 200; i++) {
            if (!fakeUpstream.isAlive() && !out.ready()) {
                break;
            }
            String line = out.readLine();
            if (line == null) {
                break;
            }
            if (line.startsWith("URL ")) {
                url = line.substring("URL ".length());
            } else if (line.startsWith("CONFIG ")) {
                configJson = line.substring("CONFIG ".length());
                break;
            }
        }
        assumeTrue(configJson != null, "the fake upstream never announced a CONFIG");
        generatedUrl = url + "/generated";
        llmux = LlmuxDirect.open(library, configJson);
    }

    @AfterAll
    static void stopFakeUpstreamAndGateway() {
        if (llmux != null) {
            llmux.close();
        }
        if (fakeUpstream != null) {
            fakeUpstream.destroy();
        }
    }

    private static String streamRequest() {
        return "{\"model\":\"demo\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}";
    }

    // -- cancel() ------------------------------------------------------------

    @Test
    void cancelFromAnotherThreadFailsTheBlockedStreamWithoutClosingIt() throws Exception {
        AtomicInteger seen = new AtomicInteger();
        CountDownLatch threeChunks = new CountDownLatch(1);
        AtomicReference<Throwable> failure = new AtomicReference<>();

        Thread worker = new Thread(() -> {
            try {
                llmux.stream("chat", streamRequest(), chunk -> {
                    if (seen.incrementAndGet() == 3) {
                        threeChunks.countDown();
                    }
                    return true;
                });
            } catch (Throwable t) {
                failure.set(t);
            }
        });
        worker.start();
        assertTrue(threeChunks.await(5, TimeUnit.SECONDS), "never saw 3 chunks");

        llmux.cancel(); // fact 1 in the shared brief: this is a FAILURE return, not a clean stop
        worker.join(TimeUnit.SECONDS.toMillis(5));
        assertFalse(worker.isAlive(), "worker thread did not stop after cancel()");

        // The blocked stream() call fails — a cancelled stream is not the
        // same outcome as a handler returning false.
        assertTrue(failure.get() instanceof LlmuxException, "expected LlmuxException, got " + failure.get());
        assertTrue(failure.get().getMessage().contains("context canceled"), failure.get().getMessage());

        // Cancellation stopped generation partway: strictly fewer than the
        // full ten words were produced, and at least the three delivered.
        int generated = generatedCount();
        assertTrue(generated >= 3 && generated < 12, "generated=" + generated);

        // Fact 3: the handle survives. A plain call still works afterward.
        String models = llmux.call("models", null);
        assertTrue(models.contains("\"object\":\"list\""), models);
    }

    @Test
    void cancelOnAnIdleHandleIsANoOp() {
        // Nothing is running on this handle right now (the previous test's
        // stream already finished). llmux_cancel on an idle handle is
        // documented as a no-op; calling it twice is too.
        llmux.cancel();
        llmux.cancel();
        String models = llmux.call("models", null);
        assertTrue(models.contains("\"object\":\"list\""), models);
    }

    // -- interruption ----------------------------------------------------

    @Test
    void interruptingTheBlockedThreadThrowsInterruptedExceptionAndClearsInterruptStatus() throws Exception {
        AtomicInteger seen = new AtomicInteger();
        CountDownLatch threeChunks = new CountDownLatch(1);
        AtomicReference<Throwable> failure = new AtomicReference<>();
        AtomicReference<Boolean> interruptStatusAfterCatch = new AtomicReference<>();

        Thread worker = new Thread(() -> {
            try {
                llmux.stream("chat", streamRequest(), chunk -> {
                    if (seen.incrementAndGet() == 3) {
                        threeChunks.countDown();
                    }
                    return true;
                });
            } catch (Throwable t) {
                failure.set(t);
                // Thread.sleep leaves the flag cleared when it throws
                // InterruptedException; LlmuxDirect.stream documents the same
                // convention. Check it from inside the same thread, right
                // after the catch, before anything else can touch it.
                interruptStatusAfterCatch.set(Thread.currentThread().isInterrupted());
            }
        });
        worker.start();
        assertTrue(threeChunks.await(5, TimeUnit.SECONDS), "never saw 3 chunks");

        worker.interrupt(); // the idiomatic path: no cancel() call, just Thread.interrupt()
        worker.join(TimeUnit.SECONDS.toMillis(5));
        assertFalse(worker.isAlive(), "worker thread did not stop after interrupt()");

        assertTrue(failure.get() instanceof InterruptedException, "expected InterruptedException, got " + failure.get());
        assertEquals(Boolean.FALSE, interruptStatusAfterCatch.get(), "interrupt status should be cleared, as Thread.sleep leaves it");

        // The handle survives interruption too, the same as it survives cancel().
        String models = llmux.call("models", null);
        assertTrue(models.contains("\"object\":\"list\""), models);
    }

    // -- helpers ---------------------------------------------------------

    private static int generatedCount() throws Exception {
        HttpRequest req = HttpRequest.newBuilder(URI.create(generatedUrl)).GET().build();
        String body = HTTP.send(req, HttpResponse.BodyHandlers.ofString()).body();
        String key = "\"generated\"";
        int i = body.indexOf(key);
        i = body.indexOf(':', i + key.length()) + 1;
        while (Character.isWhitespace(body.charAt(i))) {
            i++;
        }
        int end = i;
        while (end < body.length() && Character.isDigit(body.charAt(end))) {
            end++;
        }
        return Integer.parseInt(body.substring(i, end));
    }

    private static boolean onPath(String tool) {
        String path = System.getenv("PATH");
        if (path == null) {
            return false;
        }
        for (String dir : path.split(java.io.File.pathSeparator)) {
            if (Files.isExecutable(Paths.get(dir, tool))) {
                return true;
            }
        }
        return false;
    }

    private static Path here() {
        return Paths.get("").toAbsolutePath();
    }

    /** Walk up from the working directory looking for sdks/fake-upstream.py. */
    private static Path findFakeUpstreamPy() {
        for (Path at = here(); at != null; at = at.getParent()) {
            Path candidate = at.resolve("sdks").resolve("fake-upstream.py");
            if (Files.isRegularFile(candidate)) {
                return candidate;
            }
            // Also try directly under `at` for the case where cwd is already sdks/.
            Path direct = at.resolve("fake-upstream.py");
            if (Files.isRegularFile(direct)) {
                return direct;
            }
        }
        return null;
    }
}
