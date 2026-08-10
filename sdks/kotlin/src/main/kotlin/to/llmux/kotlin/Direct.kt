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
import java.util.concurrent.atomic.AtomicBoolean

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
     * Prefer [chunks] unless you are deliberately staying off coroutines. Note
     * that the underlying [to.llmux.LlmuxDirect.stream] this delegates to
     * declares a checked `InterruptedException` — Kotlin does not enforce
     * catching it, but it can still reach you here if this thread is
     * interrupted while blocked; see [to.llmux.LlmuxDirect.stream]'s
     * "Cancellation" section (in the Java SDK) for why, and use [chunks]
     * instead if you want cancellation as `CancellationException`.
     *
     * @return the number of chunks delivered
     */
    public fun stream(
        method: String = "chat",
        requestJson: String,
        onChunk: (String) -> Boolean,
    ): Int = delegate.stream(method, requestJson, onChunk)

    /**
     * Aborts every call in flight on this gateway — [call] and [stream] (and
     * every in-progress [chunks] collection) alike — without closing it. A
     * thin delegate to [to.llmux.LlmuxDirect.cancel]; read that method's doc
     * for the full contract, in particular that **it is per-HANDLE, not
     * per-call**: it also aborts a sibling [call] or [stream] running
     * concurrently on this same [LlmuxGateway]. [chunks] calls this itself
     * when its `Flow` is cancelled or abandoned, so most callers never reach
     * for it directly — it exists at this level for the same reason
     * `to.llmux.LlmuxDirect.cancel` does: something else, entirely outside any
     * one `Flow`, decided this gateway should stop.
     */
    public fun cancel(): Unit = delegate.cancel()

    /**
     * The same stream as a cold [Flow] of chunk JSON documents, cancellable
     * the way coroutines expect: cancelling the collecting coroutine, the
     * enclosing scope, or a `withTimeout` around the collection reaches
     * `llmux_cancel`, and what a cancelled collector sees is
     * [kotlinx.coroutines.CancellationException] — never a wrapped
     * `LlmuxException` complaining "context canceled" about a cancellation
     * this Flow caused on its own behalf.
     *
     * Four properties worth knowing:
     *
     * * **Cancellation reaches `llmux_cancel` directly, not just by
     *   abandonment.** [awaitClose] calls [to.llmux.LlmuxDirect.cancel]
     *   itself, rather than relying solely on the next chunk noticing the
     *   channel is closed and returning "stop" from the callback. The
     *   difference matters when the provider is *between* chunks — mid
     *   network read, no callback pending: without an explicit `cancel()`
     *   there and then, `llmux_stream` keeps blocking until the provider
     *   produces another chunk for the callback to notice on, which could be
     *   a long time. `cancel()` unblocks it immediately instead, from another
     *   thread — which its own contract calls safe.
     * * **That is why this flow is also a RENDEZVOUS, not a buffer**, which
     *   still matters independently of the point above: even with `cancel()`
     *   wired in, a *buffered* channel lets the callback keep accepting
     *   chunks the collector already stopped wanting, right up until the
     *   buffer fills. Measured against the fake upstream, on a five-chunk
     *   stream collected with `take(2)`: with `Channel.BUFFERED` the callback
     *   fired **5** times — the producer filled the 64-slot buffer before the
     *   collector ever cancelled, so `take` discarded three chunks that had
     *   already been generated and billed. With `Channel.RENDEZVOUS` it fired
     *   **3**: two collected and one in flight at the handoff. The cost is one
     *   context switch per chunk, which against a model emitting tokens is
     *   free.
     * * **The blocking call runs on [Dispatchers.IO], and this is what makes
     *   cancellation observable at all.** `llmux_stream` blocks its thread for
     *   the life of the stream — it has no suspension point of its own — so if
     *   it ran on the collector's dispatcher, cancelling the collector would
     *   have nothing to interrupt: the thread running the blocking call would
     *   never be freed up to notice. Running it on its own `Dispatchers.IO`
     *   thread means the *producer's* cancellation (in [awaitClose], which
     *   fires as soon as the collector or its scope cancels, independent of
     *   what the IO thread is doing) is what reaches `cancel()`, not something
     *   the blocked thread has to cooperate with on its own.
     * * A cancellation this `Flow` caused on itself — via `awaitClose` — is
     *   swallowed rather than handed to the collector as a `LlmuxException`:
     *   `awaitClose` sets a flag before calling `cancel()`, and the internal
     *   job's catch block checks that flag before deciding whether to close
     *   the channel with the resulting error or cleanly. An *external*,
     *   unrelated call to [cancel] — fact 6 in the C ABI's own docs,
     *   "per-handle, not per-call" — still surfaces as a real
     *   `LlmuxException`, because from this `Flow`'s perspective that is a
     *   genuine disruption, not its own shutdown.
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
        // Set BEFORE cancel() below, not inferred afterward from isActive: a
        // flag written first and read once, on the thread that is actually
        // deciding, is simpler to reason about than a race between two
        // notifications (job cancellation, native call unwinding) landing in
        // an unpredictable order.
        val cancelledByAwaitClose = AtomicBoolean(false)

        // llmux_stream blocks, so it gets its own IO thread; the flow's
        // producer scope stays free to observe cancellation.
        val job = launch(Dispatchers.IO) {
            try {
                delegate.stream(method, requestJson) { chunk ->
                    // Backpressure with a stop signal: a failed send means the
                    // collector is gone, which is exactly when to stop. This
                    // is the FALLBACK path — cancel() below is the primary
                    // one — for the case where a chunk was already in flight
                    // when cancellation was requested.
                    trySendBlocking(chunk).isSuccess
                }
                close()
            } catch (t: Throwable) {
                // llmux_cancel has no concept of Kotlin cancellation: our own
                // call to it below surfaces here as an ordinary
                // LlmuxException ("context canceled"), exactly as an
                // unrelated external cancel() would. Distinguish them by
                // whether WE asked for this: if awaitClose already fired, the
                // structured-concurrency machinery is already delivering
                // CancellationException to the collector on its own, and
                // handing it a second, different exception through the
                // channel would replace that with a confusing wrapped error.
                if (cancelledByAwaitClose.get()) close() else close(t)
            }
        }
        awaitClose {
            cancelledByAwaitClose.set(true)
            // Reach llmux_cancel directly — see the third bullet above — then
            // cancel the coroutine job for ordinary structured-concurrency
            // bookkeeping. Order matters: the flag must be visible before the
            // native call can possibly fail because of it.
            delegate.cancel()
            job.cancel()
        }
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
