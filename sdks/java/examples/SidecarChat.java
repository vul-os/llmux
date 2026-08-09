import to.llmux.Llmux;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.stream.Stream;

/**
 * llmux SIDECAR — the gateway as a child process on 127.0.0.1, spoken to over
 * HTTP. This is the recommended default for the JVM.
 *
 * Nothing here is llmux-specific beyond the spawn: once Llmux.baseUrl() has
 * given you a URL, any OpenAI-compatible client works against it. This example
 * uses java.net.http so it has no dependencies at all.
 *
 * Run it with sdks/java/run-examples.sh, which builds the gateway binary and
 * starts a fake upstream so no API key is needed.
 *
 * Works on any Java 11+. No FFM, no native library, no signal handlers.
 */
public final class SidecarChat {

    public static void main(String[] args) throws Exception {
        // Llmux.start() spawns the binary, waits for /health, and registers a
        // shutdown hook. The try/finally is belt and braces: it stops the child
        // on the error path too, without waiting for JVM exit.
        String base = Llmux.baseUrl();
        try {
            System.out.println("sidecar: " + base);
            System.out.println("openai base url: " + Llmux.openaiBaseUrl());

            HttpClient http = HttpClient.newBuilder()
                    .connectTimeout(Duration.ofSeconds(5))
                    .build();

            // 1. The model list.
            HttpResponse<String> models = http.send(
                    HttpRequest.newBuilder(URI.create(base + "/v1/models")).GET().build(),
                    HttpResponse.BodyHandlers.ofString());
            System.out.println("models: " + models.statusCode() + " " + oneLine(models.body(), 160));

            // 2. One chat completion.
            String request = "{"
                    + "\"model\":\"demo\","
                    + "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]"
                    + "}";
            HttpResponse<String> chat = http.send(
                    HttpRequest.newBuilder(URI.create(base + "/v1/chat/completions"))
                            .header("Content-Type", "application/json")
                            .POST(HttpRequest.BodyPublishers.ofString(request))
                            .build(),
                    HttpResponse.BodyHandlers.ofString());
            System.out.println("chat: " + chat.statusCode() + " " + oneLine(chat.body(), 200));

            // 3. The same thing streamed, over SSE. The direct path hands you
            //    chunks through a callback; here they arrive as `data:` lines.
            String streamRequest = "{"
                    + "\"model\":\"demo\","
                    + "\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],"
                    + "\"stream\":true"
                    + "}";
            HttpResponse<Stream<String>> sse = http.send(
                    HttpRequest.newBuilder(URI.create(base + "/v1/chat/completions"))
                            .header("Content-Type", "application/json")
                            .header("Accept", "text/event-stream")
                            .POST(HttpRequest.BodyPublishers.ofString(streamRequest))
                            .build(),
                    HttpResponse.BodyHandlers.ofLines());

            StringBuilder text = new StringBuilder();
            int[] chunks = {0};
            // The body stream holds the connection open; close it when done.
            try (Stream<String> lines = sse.body()) {
                lines.forEach(line -> {
                    if (!line.startsWith("data: ")) {
                        return;
                    }
                    String payload = line.substring("data: ".length());
                    if (payload.equals("[DONE]")) {
                        return;
                    }
                    chunks[0]++;
                    text.append(content(payload));
                });
            }
            System.out.println("stream: " + chunks[0] + " chunks, text=\"" + text + "\"");

            // 4. The error path: a model nothing routes.
            HttpResponse<String> bad = http.send(
                    HttpRequest.newBuilder(URI.create(base + "/v1/chat/completions"))
                            .header("Content-Type", "application/json")
                            .POST(HttpRequest.BodyPublishers.ofString(
                                    "{\"model\":\"nope\",\"messages\":[]}"))
                            .build(),
                    HttpResponse.BodyHandlers.ofString());
            System.out.println("error path: " + bad.statusCode() + " " + oneLine(bad.body(), 160));
        } finally {
            Llmux.stop();
            System.out.println("sidecar stopped");
        }
        System.out.println("done");
    }

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
