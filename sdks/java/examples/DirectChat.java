import to.llmux.LlmuxDirect;
import to.llmux.LlmuxException;

import java.nio.file.Path;

/**
 * llmux DIRECT — the gateway running inside this JVM, through libllmux's C ABI.
 *
 * Run it with sdks/java/run-examples.sh, which builds the shared library,
 * starts the fake upstream from ffi/fakeupstream, and passes its configuration
 * in LLMUX_CONFIG_JSON so this example needs no API key.
 *
 * Requires Java 22+ (java.lang.foreign) and --enable-native-access=ALL-UNNAMED.
 *
 * READ sdks/java/README.md FIRST. On the JVM the sidecar is the recommended
 * default, for reasons that are measured there rather than asserted.
 */
public final class DirectChat {

    public static void main(String[] args) throws InterruptedException {
        Path library = LlmuxDirect.findLibrary();
        System.out.println("library: " + library);

        String config = System.getenv("LLMUX_CONFIG_JSON");
        boolean haveUpstream = config != null && !config.isEmpty();

        // try-with-resources: the handle is closed on every path out of this
        // block, including the exceptional ones.
        try (LlmuxDirect llmux = LlmuxDirect.open(library, config)) {
            System.out.println("abi version: " + llmux.abiVersion());

            // 1. A call that needs no upstream at all.
            String models = llmux.call("models", null);
            System.out.println("models: " + oneLine(models, 160));

            if (!haveUpstream) {
                System.out.println();
                System.out.println("LLMUX_CONFIG_JSON is unset, so there is no upstream to talk to.");
                System.out.println("Skipping the chat and stream calls. Use run-examples.sh to get one.");
                return;
            }

            // 2. One unary chat completion.
            String request = "{"
                    + "\"model\":\"demo\","
                    + "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]"
                    + "}";
            String answer = llmux.call("chat", request);
            System.out.println("chat: " + oneLine(answer, 200));

            // 3. The same completion, streamed. The handler runs on THIS
            //    thread, synchronously, before stream() returns.
            Thread caller = Thread.currentThread();
            StringBuilder text = new StringBuilder();
            boolean[] offThread = {false};

            String streamRequest = "{"
                    + "\"model\":\"demo\","
                    + "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],"
                    + "\"stream\":true"
                    + "}";
            int chunks = llmux.stream("chat", streamRequest, chunk -> {
                if (Thread.currentThread() != caller) {
                    offThread[0] = true;
                }
                text.append(content(chunk));
                return true; // keep streaming
            });
            System.out.println("stream: " + chunks + " chunks, text=\"" + text + "\"");
            System.out.println("callback ran on the calling thread: " + !offThread[0]);

            // 4. Stopping early. Returning false is a decision, not a failure.
            int[] seen = {0};
            int delivered = llmux.stream("chat", streamRequest, chunk -> {
                seen[0]++;
                return seen[0] < 2; // stop after the second chunk
            });
            System.out.println("early stop: delivered " + delivered + " chunk(s), no exception");

            // 5. The error path. A bad method is a clean error string, and the
            //    string the library allocated for it is freed by the binding.
            try {
                llmux.call("no-such-method", "{}");
                System.out.println("FAIL: a bogus method should not have succeeded");
            } catch (LlmuxException e) {
                System.out.println("error path: " + e.getMessage());
            }
        }

        // 6. Use after close is a clean error too, not a crash.
        LlmuxDirect closed = LlmuxDirect.open(library, config);
        closed.close();
        closed.close(); // idempotent
        try {
            closed.call("models", null);
            System.out.println("FAIL: a closed handle should not have answered");
        } catch (LlmuxException e) {
            System.out.println("after close: " + e.getMessage());
        }

        System.out.println("done");
    }

    /** Pull the assistant text out of a chunk without linking a JSON parser. */
    private static String content(String chunkJson) {
        String needle = "\"content\":\"";
        int i = chunkJson.indexOf(needle);
        if (i < 0) {
            return "";
        }
        i += needle.length();
        StringBuilder out = new StringBuilder();
        while (i < chunkJson.length() && chunkJson.charAt(i) != '"') {
            char c = chunkJson.charAt(i);
            if (c == '\\' && i + 1 < chunkJson.length()) {
                i++;
                c = chunkJson.charAt(i) == 'n' ? '\n' : chunkJson.charAt(i);
            }
            out.append(c);
            i++;
        }
        return out.toString();
    }

    private static String oneLine(String s, int max) {
        String flat = s.replaceAll("\\s+", " ");
        return flat.length() <= max ? flat : flat.substring(0, max) + "…";
    }
}
