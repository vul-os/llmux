# llmux (PHP)

Use llmux **locally** from PHP. The package bundles the gateway binary, starts
it on a local port via `proc_open`, and hands your existing OpenAI client a
`base_url`.

```php
use Llmux\Llmux;

$base = Llmux::baseUrl();        // http://127.0.0.1:<port>
$v1   = Llmux::openaiBaseUrl();  // http://127.0.0.1:<port>/v1

// Or a configured openai-php client (optional openai-php/client):
$client = Llmux::openai();
$r = $client->chat()->create([
    'model'    => 'anthropic/claude-3-5-sonnet',
    'messages' => [['role' => 'user', 'content' => 'hi']],
]);
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
at process shutdown.

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `bin/`:

```sh
go build -o sdks/php/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).
