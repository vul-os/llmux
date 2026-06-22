# llmux (Java)

Use llmux **locally** from Java. The library bundles the gateway binary, starts
it on a local port via `ProcessBuilder`, polls health with `java.net.http`, and
hands your OpenAI-compatible client a `base_url`. Core needs only the JDK.

```java
import to.llmux.Llmux;

String base = Llmux.baseUrl();        // http://127.0.0.1:<port>
String v1 = Llmux.openaiBaseUrl();    // http://127.0.0.1:<port>/v1
```

Convenience: build an `openai-java` client pointed at the gateway (optional
`com.openai:openai-java` dependency):

```java
import com.openai.client.OpenAIClient;
import com.openai.client.okhttp.OpenAIOkHttpClient;

OpenAIClient client = OpenAIOkHttpClient.builder()
    .baseUrl(Llmux.openaiBaseUrl())
    .apiKey("llmux-local")
    .build();
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
via a JVM shutdown hook.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (a sibling `bin/` next to the jar/classes, or
   `LLMUX_HOME/bin/llmux`; `llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `bin/`:

```sh
go build -o sdks/java/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).
