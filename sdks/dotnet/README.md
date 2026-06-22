# llmux (.NET)

Use llmux **locally** from C#/.NET. The package bundles the gateway binary,
starts it on a local port via `System.Diagnostics.Process`, polls health with
`HttpClient`, and hands your OpenAI-compatible client a `base_url`.

```csharp
using Llmux;

string baseUrl = Sidecar.BaseUrl();        // http://127.0.0.1:<port>
string v1 = Sidecar.OpenAIBaseUrl();       // …/v1
```

Convenience: point the official `OpenAI` nuget at the gateway (optional):

```csharp
using OpenAI;
using OpenAI.Chat;

var client = new OpenAIClient(
    new System.ClientModel.ApiKeyCredential("llmux-local"),
    new OpenAIClientOptions { Endpoint = new Uri(Sidecar.OpenAIBaseUrl()) });
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
on `ProcessExit` / Ctrl-C.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` next to the assembly (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `bin/`:

```sh
go build -o sdks/dotnet/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).
