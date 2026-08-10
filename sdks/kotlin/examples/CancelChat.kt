import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeout
import to.llmux.kotlin.Llmux
import to.llmux.kotlin.LlmuxGateway
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.util.concurrent.atomic.AtomicInteger

/**
 * llmux CANCELLATION from Kotlin — structured concurrency reaching
 * llmux_cancel through [LlmuxGateway.chunks]'s [kotlinx.coroutines.flow.Flow].
 *
 * DirectChat.kt's "early stop" demo (`take(2)`) shows the Flow's own natural
 * completion path. This example is about EXTERNAL cancellation instead — a
 * coroutine, scope, or timeout deciding from outside that a collection in
 * progress should stop — because that path has two things worth proving
 * rather than assuming:
 *
 *   1. that it reaches `llmux_cancel` at all, not just "stops looking"; and
 *   2. that what the collector sees is [CancellationException], not a wrapped
 *      `LlmuxException` about the cancellation this SDK caused on its own
 *      behalf — see [LlmuxGateway.chunks]'s doc for why that distinction
 *      needs code, not just hope.
 *
 * Run with sdks/kotlin/run-examples.sh cancel, which starts
 * sdks/fake-upstream.py — the only fake upstream in this repo that delays
 * each chunk (so there is time to cancel mid-stream) and counts, via
 * GET /generated, what it actually produced.
 *
 * Requires Java 22+ and --enable-native-access=ALL-UNNAMED.
 */
fun main() = runBlocking {
    val library = Llmux.findLibrary()
    println("library: $library")

    val config: String? = System.getenv("LLMUX_CONFIG_JSON")?.ifEmpty { null }
    if (config == null) {
        println()
        println("LLMUX_CONFIG_JSON is unset, so there is no upstream to cancel against.")
        println("Run this with sdks/kotlin/run-examples.sh cancel.")
        return@runBlocking
    }

    val generatedUrl = baseUrl(config) + "/generated"
    val http = HttpClient.newHttpClient()
    val streamRequest =
        """{"model":"demo","messages":[{"role":"user","content":"hello"}],"stream":true}"""

    Llmux.direct(configJson = config, library = library).use { llmux ->
        val before = generatedCount(http, generatedUrl)

        val consumerJobCancel = cancelViaJobCancel(llmux, streamRequest)
        val afterJobCancel = generatedCount(http, generatedUrl)
        println("job.cancel(): consumer saw $consumerJobCancel chunk(s); " +
            "upstream generated ${afterJobCancel - before} for this run")

        val consumerTimeout = cancelViaWithTimeout(llmux, streamRequest, chunkDelayMs = 100)
        val afterTimeout = generatedCount(http, generatedUrl)
        println("withTimeout: consumer saw $consumerTimeout chunk(s); " +
            "upstream generated ${afterTimeout - afterJobCancel} for this run")

        // Neither cancellation closed the handle.
        val models = llmux.call("models")
        println("handle survives both cancellations: models call returned ${models.length} bytes")
    }

    println("done")
}

/**
 * Cancel the collecting coroutine directly, once it has seen 3 chunks, and
 * confirm what propagates out of `collect` is [CancellationException] — not
 * the `LlmuxException("context canceled")` that `llmux_stream` itself fails
 * with underneath. [CompletableDeferred] is the signal from the collector
 * back to this function that it is time to cancel; [kotlinx.coroutines.Job.join]
 * is what actually waits for the coroutine to finish unwinding, the same
 * reason `examples/CancelChat.java` waits on `Thread.join`/
 * `ExecutorService.awaitTermination` rather than trusting that cancelling
 * something means it has already stopped.
 */
private suspend fun cancelViaJobCancel(llmux: LlmuxGateway, streamRequest: String): Int = coroutineScope {
    val seen = AtomicInteger()
    val threeChunks = CompletableDeferred<Unit>()
    var caught: Throwable? = null

    val job = launch {
        try {
            llmux.chunks(requestJson = streamRequest).collect {
                if (seen.incrementAndGet() == 3) {
                    threeChunks.complete(Unit)
                }
            }
        } catch (t: Throwable) {
            caught = t
            throw t // a cancellation must be rethrown, never swallowed silently
        }
    }

    threeChunks.await()
    job.cancel() // structured-concurrency cancellation: no cancel() call of our own here
    job.join()

    check(caught is CancellationException) { "expected CancellationException, got $caught" }
    seen.get()
}

/**
 * The other construct the shared brief names explicitly: a [withTimeout]
 * around the collection. There is no per-chunk signal here — the timeout is
 * sized to elapse shortly after the 3rd of ten 100 ms chunks and before the
 * 4th — so what this proves is that a plain deadline, not just an explicit
 * `job.cancel()`, also reaches `llmux_cancel` and also surfaces as
 * [TimeoutCancellationException] (a [CancellationException] subtype), not a
 * wrapped llmux error.
 */
private suspend fun cancelViaWithTimeout(llmux: LlmuxGateway, streamRequest: String, chunkDelayMs: Long): Int {
    val seen = AtomicInteger()
    var caught: Throwable? = null
    try {
        withTimeout(chunkDelayMs * 3 + chunkDelayMs / 2) {
            llmux.chunks(requestJson = streamRequest).collect {
                seen.incrementAndGet()
            }
        }
    } catch (t: Throwable) {
        caught = t
    }
    check(caught is TimeoutCancellationException) { "expected TimeoutCancellationException, got $caught" }
    return seen.get()
}

/** GET /generated from the fake upstream and pull out the "generated" count. */
private suspend fun generatedCount(http: HttpClient, url: String): Int = withContext(Dispatchers.IO) {
    val req = HttpRequest.newBuilder(URI.create(url)).GET().build()
    val body = http.send(req, HttpResponse.BodyHandlers.ofString()).body()
    val key = "\"generated\""
    var i = body.indexOf(key)
    check(i >= 0) { "no \"generated\" field in $body" }
    i = body.indexOf(':', i + key.length) + 1
    while (body[i].isWhitespace()) i++
    var end = i
    while (end < body.length && body[end].isDigit()) end++
    body.substring(i, end).toInt()
}

/**
 * The fake upstream's config document names itself as
 * `"base_url": "http://127.0.0.1:PORT/v1"`. Strip the `/v1` to get back the
 * plain HTTP server that also answers GET /generated. Tolerant of the space
 * after `:` that `json.dumps`'s default separators produce.
 */
private fun baseUrl(configJson: String): String {
    val key = "\"base_url\""
    var i = configJson.indexOf(key)
    check(i >= 0) { "no \"base_url\" in config json: $configJson" }
    i = configJson.indexOf(':', i + key.length) + 1
    while (configJson[i].isWhitespace()) i++
    check(configJson[i] == '"') { "\"base_url\" is not a JSON string in: $configJson" }
    i++
    val end = configJson.indexOf('"', i)
    val withV1 = configJson.substring(i, end)
    return if (withV1.endsWith("/v1")) withV1.removeSuffix("/v1") else withV1
}
