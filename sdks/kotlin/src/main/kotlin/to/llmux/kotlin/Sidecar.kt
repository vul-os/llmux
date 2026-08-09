@file:JvmName("LlmuxSidecarKt")

package to.llmux.kotlin

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.flowOn
import kotlinx.coroutines.future.await
import to.llmux.Llmux as JavaLlmux
import java.net.URI
import java.net.http.HttpClient
import java.net.http.HttpRequest
import java.net.http.HttpResponse
import java.time.Duration
import java.util.stream.Stream

/**
 * Kotlin over [to.llmux.Llmux] — llmux as a child process on `127.0.0.1`,
 * spoken to over HTTP.
 *
 * **This is the recommended default on the JVM**, for reasons set out in
 * `README.md`. It needs no native library, no `--enable-native-access`, and it
 * runs on every platform llmux builds for, Windows included.
 *
 * ```kotlin
 * LlmuxSidecar().use { llmux ->
 *     println(llmux.models())
 *     llmux.chatChunks(requestJson).collect { print(it) }
 * }
 * ```
 *
 * The underlying process is a singleton owned by the Java class and is torn
 * down by a JVM shutdown hook; [close] stops it earlier, which is what `use {}`
 * gets you.
 */
public class LlmuxSidecar(
    fixedPort: Int? = null,
    configPath: String? = null,
    extraEnv: Map<String, String>? = null,
    healthTimeout: Duration = Duration.ofSeconds(10),
) : AutoCloseable {

    /** `http://127.0.0.1:<port>` — the running gateway. */
    public val baseUrl: String

    /** `…/v1` — hand this to any OpenAI-compatible client. */
    public val openAiBaseUrl: String get() = "$baseUrl/v1"

    private val http: HttpClient = HttpClient.newBuilder()
        .connectTimeout(Duration.ofSeconds(5))
        .build()

    init {
        val opts = JavaLlmux.Options()
        opts.port = fixedPort
        opts.config = configPath
        opts.env = extraEnv
        opts.timeout = healthTimeout
        baseUrl = JavaLlmux.start(opts)
    }

    /** `GET /v1/models`. */
    public suspend fun models(): String = get("/v1/models")

    /** `POST /v1/chat/completions`, non-streaming. */
    public suspend fun chat(requestJson: String): String =
        post("/v1/chat/completions", requestJson)

    /**
     * `POST /v1/chat/completions` with `"stream": true`, as a [Flow] of chunk
     * JSON documents — the same documents the direct path delivers to a
     * callback, with the `data: ` prefix and the terminal `[DONE]` frame
     * already stripped.
     *
     * The request body must already ask for streaming; this does not edit your
     * JSON.
     *
     * Cancelling the collector closes the response body, which drops the
     * connection and stops the server generating.
     */
    public fun chatChunks(requestJson: String): Flow<String> = flow {
        val response = http.send(
            HttpRequest.newBuilder(URI.create("$baseUrl/v1/chat/completions"))
                .header("Content-Type", "application/json")
                .header("Accept", "text/event-stream")
                .POST(HttpRequest.BodyPublishers.ofString(requestJson))
                .build(),
            HttpResponse.BodyHandlers.ofLines(),
        )
        // The line stream owns the connection; close it however we leave.
        response.body().use { lines: Stream<String> ->
            for (line in lines) {
                if (!line.startsWith(DATA_PREFIX)) continue
                val payload = line.substring(DATA_PREFIX.length)
                if (payload == "[DONE]") break
                emit(payload)
            }
        }
    }.flowOn(Dispatchers.IO)

    private suspend fun get(path: String): String =
        http.sendAsync(
            HttpRequest.newBuilder(URI.create(baseUrl + path)).GET().build(),
            HttpResponse.BodyHandlers.ofString(),
        ).await().body()

    private suspend fun post(path: String, body: String): String =
        http.sendAsync(
            HttpRequest.newBuilder(URI.create(baseUrl + path))
                .header("Content-Type", "application/json")
                .POST(HttpRequest.BodyPublishers.ofString(body))
                .build(),
            HttpResponse.BodyHandlers.ofString(),
        ).await().body()

    /** Stop the child process. Idempotent. */
    override fun close(): Unit = JavaLlmux.stop()

    private companion object {
        const val DATA_PREFIX = "data: "
    }
}
