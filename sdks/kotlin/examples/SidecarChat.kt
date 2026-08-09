import kotlinx.coroutines.flow.take
import kotlinx.coroutines.flow.toList
import kotlinx.coroutines.runBlocking
import to.llmux.kotlin.LlmuxSidecar

/**
 * llmux SIDECAR from Kotlin — the gateway as a child process on 127.0.0.1.
 *
 * This is the recommended default for the JVM: no native library, no
 * --enable-native-access, no signal handlers, and it works on every platform
 * llmux builds for, Windows included.
 *
 * Runs on any Java 11+. Run it with sdks/kotlin/run-examples.sh.
 */
fun main() = runBlocking {
    // use {} stops the child process on every path out, rather than waiting for
    // the JVM shutdown hook.
    LlmuxSidecar().use { llmux ->
        println("sidecar: ${llmux.baseUrl}")
        println("openai base url: ${llmux.openAiBaseUrl}")

        println("models: ${llmux.models().oneLine(160)}")

        val request = """{"model":"demo","messages":[{"role":"user","content":"hello"}]}"""
        println("chat: ${llmux.chat(request).oneLine(200)}")

        // The same Flow shape the direct path offers, over SSE instead of a
        // callback. Swapping one for the other is a one-line change at the
        // construction site — that is the point of sharing the wire contract.
        val streamRequest =
            """{"model":"demo","messages":[{"role":"user","content":"hello"}],"stream":true}"""
        val chunks = llmux.chatChunks(streamRequest).toList()
        val text = chunks.joinToString("") { it.content() }
        println("stream: ${chunks.size} chunks, text=\"$text\"")

        // Cancelling unwinds through the `use` that owns the response body, so
        // the connection is closed. How many tokens the server had already
        // produced by then is not observable from here — unlike the direct
        // path, where the callback count is exact. That asymmetry is real and
        // worth knowing before you rely on early cancellation to save spend.
        val firstTwo = llmux.chatChunks(streamRequest).take(2).toList()
        println("early stop: collected ${firstTwo.size} chunk(s), response body closed")

        // The error path is an HTTP status, not an exception.
        val bad = llmux.chat("""{"model":"nope","messages":[]}""")
        println("error path: ${bad.oneLine(160)}")
    }
    println("sidecar stopped")
    println("done")
}

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
