import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import to.llmux.LlmuxException
import to.llmux.kotlin.Llmux

/**
 * llmux DIRECT from Kotlin — the gateway inside this JVM, through libllmux.
 *
 * Run with sdks/kotlin/run-examples.sh, which builds the shared library, starts
 * ffi/fakeupstream, and passes its configuration in LLMUX_CONFIG_JSON.
 *
 * Requires Java 22+ and --enable-native-access=ALL-UNNAMED.
 *
 * READ sdks/kotlin/README.md FIRST: on the JVM the sidecar is the recommended
 * default, and the reasons are measured rather than asserted.
 */
fun main() = runBlocking {
    val library = Llmux.findLibrary()
    println("library: $library")

    val config: String? = System.getenv("LLMUX_CONFIG_JSON")?.ifEmpty { null }

    // use {} closes the handle on every path out, including a thrown exception.
    Llmux.direct(configJson = config, library = library).use { llmux ->
        println("abi version: ${llmux.abiVersion}")

        // 1. No upstream needed.
        println("models: ${llmux.call("models").oneLine(160)}")

        if (config == null) {
            println()
            println("LLMUX_CONFIG_JSON is unset, so there is no upstream to talk to.")
            println("Skipping chat and streaming. Use run-examples.sh to get one.")
            return@use
        }

        val request = """{"model":"demo","messages":[{"role":"user","content":"hello"}]}"""
        println("chat: ${llmux.call("chat", request).oneLine(200)}")

        // 2. Streaming as a Flow. Chunks arrive as chat.completion.chunk JSON.
        val streamRequest =
            """{"model":"demo","messages":[{"role":"user","content":"hello"}],"stream":true}"""
        val chunks = llmux.chunks(requestJson = streamRequest).toList()
        val text = chunks.joinToString("") { it.content() }
        println("stream: ${chunks.size} chunks, text=\"$text\"")

        // 3. Cancelling the collector stops the Go stream. take(2) is not a
        //    filter applied after the fact: the failed send tells the callback
        //    to stop and llmux_stream unwinds. Measured on this same 5-chunk
        //    stream, the callback fires 3 times (two collected, one in flight)
        //    — and it fires all 5 if chunks() is given a buffer instead of a
        //    rendezvous, which is why it is not. See LlmuxGateway.chunks.
        val firstTwo = llmux.chunks(requestJson = streamRequest).take(2).toList()
        println("early stop: collected ${firstTwo.size} chunk(s); the stream unwound early")

        // 4. The error path. The library's message comes back as an exception,
        //    and the C string it allocated for it is freed by the binding.
        try {
            llmux.call("no-such-method", "{}")
            println("FAIL: a bogus method should not have succeeded")
        } catch (e: LlmuxException) {
            println("error path: ${e.message}")
        }
    }

    // 5. Use after close is a clean error, not a crash.
    val closed = Llmux.direct(configJson = config, library = library)
    closed.close()
    closed.close() // idempotent
    try {
        closed.call("models")
        println("FAIL: a closed handle should not have answered")
    } catch (e: LlmuxException) {
        println("after close: ${e.message}")
    }

    println("done")
}

/** Pull assistant text out of a chunk without adding a JSON dependency. */
private fun String.content(): String {
    val needle = "\"content\":\""
    var i = indexOf(needle)
    if (i < 0) return ""
    i += needle.length
    val out = StringBuilder()
    while (i < length && this[i] != '"') {
        var c = this[i]
        if (c == '\\' && i + 1 < length) {
            i++
            c = if (this[i] == 'n') '\n' else this[i]
        }
        out.append(c)
        i++
    }
    return out.toString()
}

private fun String.oneLine(max: Int): String {
    val flat = replace(Regex("\\s+"), " ")
    return if (flat.length <= max) flat else flat.take(max) + "…"
}
