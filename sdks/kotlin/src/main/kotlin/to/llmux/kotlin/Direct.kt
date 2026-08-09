@file:JvmName("LlmuxDirectKt")

package to.llmux.kotlin

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.channels.trySendBlocking
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.buffer
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.flowOn
import kotlinx.coroutines.launch
import to.llmux.LlmuxDirect
import java.nio.file.Path

/**
 * Kotlin over [to.llmux.LlmuxDirect] — llmux running **in this JVM** through
 * libllmux's C ABI.
 *
 * This is a thin, idiomatic layer, not a reimplementation. The FFM binding, the
 * memory rules and the handle lifecycle all live in the Java class; what Kotlin
 * adds is `use {}`, named arguments, and a [Flow] for streaming that cancels
 * the underlying Go stream when the collector stops caring.
 *
 * **The sidecar is the recommended default on the JVM.** See `README.md` →
 * "The JVM and Go's signal handlers"; the argument is measured, and it applies
 * to Kotlin exactly as it does to Java, because it is an argument about the
 * process, not the language.
 *
 * ```kotlin
 * Llmux.direct(configJson).use { llmux ->
 *     println(llmux.call("models"))
 *     llmux.chunks("chat", requestJson).collect { print(it) }
 * }
 * ```
 *
 * Requires Java 22+ and `--enable-native-access=ALL-UNNAMED`.
 */
public class LlmuxGateway internal constructor(
    private val delegate: LlmuxDirect,
) : AutoCloseable {

    /** The llmux version the loaded shared library was built from. */
    public val abiVersion: String get() = delegate.abiVersion()

    /** The library this gateway is running out of. */
    public val libraryPath: Path get() = delegate.libraryPath()

    /**
     * One unary call. [method] is `"chat"`, `"embed"` or `"models"`.
     *
     * @throws to.llmux.LlmuxException carrying the library's own message
     */
    public fun call(method: String, requestJson: String? = null): String =
        delegate.call(method, requestJson)

    /**
     * Stream a chat completion, blocking this thread and invoking [onChunk]
     * once per chunk on it. Return `false` from [onChunk] to stop; stopping is
     * a decision, not an error.
     *
     * Prefer [chunks] unless you are deliberately staying off coroutines.
     *
     * @return the number of chunks delivered
     */
    public fun stream(
        method: String = "chat",
        requestJson: String,
        onChunk: (String) -> Boolean,
    ): Int = delegate.stream(method, requestJson, onChunk)

    /**
     * The same stream as a cold [Flow] of chunk JSON documents.
     *
     * Three properties worth knowing:
     *
     * * **Cancelling the collector stops the Go stream.** When the flow's
     *   channel closes, the send from the callback fails, the callback returns
     *   "stop", and `llmux_stream` unwinds — llmux is not left producing tokens
     *   into a void.
     * * **That is why this flow is a RENDEZVOUS, not a buffer**, and it is the
     *   one design decision here that is not cosmetic. Measured against the
     *   fake upstream, on a five-chunk stream collected with `take(2)`:
     *   with `Channel.BUFFERED` the callback fired **5** times — the producer
     *   filled the 64-slot buffer before the collector ever cancelled, so
     *   `take` discarded three chunks that had already been generated and
     *   billed. With `Channel.RENDEZVOUS` it fired **3**: two collected and one
     *   in flight at the handoff. Early cancellation only means anything with
     *   backpressure, so this flow has it. The cost is one context switch per
     *   chunk, which against a model emitting tokens is free.
     * * **The blocking call runs on [Dispatchers.IO].** `llmux_stream` blocks
     *   its thread for the life of the stream; that thread must not be a
     *   coroutine dispatcher thread doing other work.
     *
     * Every chunk is one `chat.completion.chunk` object — the same JSON the
     * HTTP API writes after `data: `. Note that one chunk beyond your last
     * collected one may already have been generated: cancellation is prompt,
     * not retroactive.
     */
    public fun chunks(
        method: String = "chat",
        requestJson: String,
    ): Flow<String> = callbackFlow {
        // llmux_stream blocks, so it gets its own IO thread; the flow's
        // producer scope stays free to observe cancellation.
        val job = launch(Dispatchers.IO) {
            try {
                delegate.stream(method, requestJson) { chunk ->
                    // Backpressure with a stop signal: a failed send means the
                    // collector is gone, which is exactly when to stop.
                    trySendBlocking(chunk).isSuccess
                }
                close()
            } catch (t: Throwable) {
                close(t)
            }
        }
        awaitClose { job.cancel() }
    }.buffer(Channel.RENDEZVOUS).flowOn(Dispatchers.IO)

    /** Release the gateway. Idempotent; `use {}` calls it on every path out. */
    override fun close(): Unit = delegate.close()
}

/** Entry points for both llmux paths. */
public object Llmux {

    /**
     * Open an in-process gateway.
     *
     * @param configJson an llmux configuration document, or null for built-in
     *                   defaults plus the environment
     * @param library    an explicit libllmux; defaults to
     *                   [LlmuxDirect.findLibrary]'s search
     */
    public fun direct(configJson: String? = null, library: Path? = null): LlmuxGateway =
        LlmuxGateway(
            if (library == null) LlmuxDirect.open(configJson)
            else LlmuxDirect.open(library, configJson),
        )

    /** Where the direct path would load its library from, without loading it. */
    public fun findLibrary(): Path = LlmuxDirect.findLibrary()
}
