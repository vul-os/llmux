# Quickstarts

Five short tracks. Pick the one that matches what you are: each is five minutes,
each ends somewhere real, and each links to the page that goes deep.

If you do not yet know which mode you want, read
[Choosing a mode](choosing-a-mode.md) first — it is a table and a flowchart, and
it will save you more than the five minutes it costs.

## A. You have a gateway already — just point your client at it

Nothing about your client code changes. llmux speaks the OpenAI HTTP API
verbatim, so the whole change is a base URL.

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="sk-team-a")
resp = client.chat.completions.create(
    model="assistant",                       # an alias your operator configured
    messages=[{"role": "user", "content": "hi"}],
)
print(resp.choices[0].message.content)
print(resp.usage)                            # includes per-request cost
```

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-team-a" \
  -H "Content-Type: application/json" \
  -d '{"model":"assistant","messages":[{"role":"user","content":"Hello!"}]}'
```

Three things worth knowing on day one:

- **The `model` string is the routing decision.** `"assistant"` is an alias your
  operator defined; `"anthropic/claude-3-5-sonnet"` names a provider and model
  directly. Ask your operator for the alias list, or `GET /v1/models`.
- **`"stream": true` returns byte-identical OpenAI SSE.** Your SDK's existing
  stream parser works unchanged.
- **`usage.cost` is an additive extension** on the standard usage block. OpenAI
  clients ignore it harmlessly; yours can read it.

→ [Client examples](client-examples.md) has the same call in 17+ languages ·
[API reference](api.md) has every endpoint and error code.

## B. You are shipping an application and do not want to run a server

Use the package for your language. There are **fifteen** — Go, C, C++, Rust,
Swift, Deno, Bun, Node, Python, Java, Kotlin, .NET, Ruby, PHP and Elixir — and
every one of them can resolve and supervise the gateway for you. You never start
a server by hand, and there is no port to remember.

```python
import llmux
client = llmux.OpenAI()          # spawns the gateway, returns an openai client
res = client.chat.completions.create(
    model="anthropic/claude-3-5-sonnet",
    messages=[{"role": "user", "content": "hi"}],
)
```

```javascript
const llmux = require("llmux");
const client = await llmux.OpenAI();
const res = await client.chat.completions.create({
  model: "anthropic/claude-3-5-sonnet",
  messages: [{ role: "user", content: "hi" }],
});
```

Provider credentials come from your environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …), exactly as they do for the standalone
binary — the child process inherits them.

If the package cannot find a gateway binary, point it at one:

```bash
go build -o /tmp/llmux ./cmd/llmux
export LLMUX_BINARY=/tmp/llmux
```

**No convenience constructor is required.** Where a mature OpenAI client exists
for the language, the package offers one; otherwise `openai_base_url()` (`…/v1`,
default key `llmux-local`) plus any HTTP client is always enough. That is the
whole contract, and it is identical in all fifteen.

→ [Language packages → a first call in every
language](sdks.md#a-first-call-in-every-language) has the install line, a
working snippet and a run command for each one.

## B2. Same thing, but with no child process either

Fourteen of the fifteen can also run the gateway **inside your process**: Go
imports the package, and thirteen others load a C shared library exposing six
symbols. No port, no listener, no loopback socket.

```rust
use llmux::direct::Gateway;

let gw = Gateway::open(None)?;               // defaults + environment
let req = r#"{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}"#;
println!("{}", gw.call("chat", Some(req))?);
for chunk in gw.stream(req)? { print!("{}", chunk?); }
```

Read three lines before you commit to it, because they rule it out more often
than they rule it in:

- **You build the library yourself — no release ships one.** The build is known
  to work on darwin/arm64 and linux/arm64, and in CI on linux/amd64;
  **windows/amd64 and darwin/amd64 do not exist.**
- **There is no authentication on that boundary, by design** — virtual keys and
  budgets are the sidecar's job.
- **Latency is not the reason.** ~80–92 µs against ~102–109 µs for a real chat
  call, which is noise next to the model.

Build the library with `scripts/build-ffi.sh`, then point your package at it —
`LLMUX_LIBRARY` in Python, Ruby, Rust, Swift, Java, Kotlin, .NET and PHP;
`LLMUX_LIB` in Node, Bun and Deno; the build-time `LLMUX_LIB_DIR` in C and C++.

→ [Choosing a mode](choosing-a-mode.md#your-language-has-probably-already-answered-this)
· [The C ABI](c-abi.md)

## C. You are writing Go and want no process at all

`core/gateway` is the whole dispatch path as a library. No listener, no port, no
child process — the provider call is the only socket your program opens.

```go
cfg := config.Default()          // auto-detects providers from the environment
gw, err := gateway.New(cfg)
if err != nil {
	log.Fatal(err)
}
defer gw.Close()

res, err := gw.Chat(ctx, &openai.ChatCompletionRequest{
	Model:    "gpt-4o-mini",
	Messages: []openai.Message{{Role: "user", Content: openai.Str("hi")}},
})
fmt.Println(res.Response.Choices[0].Message.Content.Text)
fmt.Println("served by", res.Provider) // who actually served, after failover
```

Read these three before you ship it:

- **`gw.Authorize(ctx, token)` is the auth check**, and it is the same one the
  HTTP shell runs. It returns `(ctx, release, err)`; `release` is never nil and
  **must always be called** or a budget reservation leaks. Use the returned
  `ctx` for the dispatch call, not the original.
- **`New` starts nothing.** `Run(ctx)` opts into the price-catalog syncer, which
  on a default config makes periodic outbound requests to two pricing hosts.
- **`New` connects and migrates eagerly if `cfg.Postgres` is set**, and reads
  `os.Getenv` for any provider configured with `api_key_env`.

→ [Embedding llmux](embedding.md) for the full API, the seams and the errors.

## D. You are the operator, self-hosting for a team

```bash
git clone https://github.com/vul-os/llmux
cd llmux
make build                        # ./dist/llmux, admin console embedded

cp llmux.example.json llmux.json  # fully commented; edit providers + keys
export LLMUX_MASTER_KEY=$(openssl rand -hex 32)
export OPENAI_API_KEY=sk-...      # or whichever providers you are wiring
./dist/llmux -config llmux.json   # gateway on :4000, dashboard at /ui
```

```bash
curl http://localhost:4000/health   # {"status":"ok"} — no auth needed
./dist/llmux models                 # models, pricing, context windows
```

Then, in order of how much it matters:

1. **Set a master key.** It gates `/admin/*` and `/metrics`. Without one, the
   gateway refuses to bind a non-loopback address unless you explicitly set
   `LLMUX_INSECURE_KEYLESS=1` — which is a development-only escape hatch.
2. **Issue virtual keys** with `budget_usd`, `rpm` and `allowed_models`. These
   are what applications hold; they are never accepted on `/admin/*`, and their
   tokens are hashed at rest.
3. **Decide your egress posture.** Non-local providers are **denied by default**
   — a provider only reaches off-box when you set `allow_egress` (or a
   `tier`) on it. Check `/health` with the master key to see the classification
   llmux actually made, rather than the one you intended.
4. **Only then** consider Postgres (cross-replica keys and spend) and Redis
   (shared rate limits and cache). One replica needs neither.

→ [Getting started](GETTING-STARTED.md) is the long form of this track ·
[Admin guide](ADMIN-GUIDE.md) for budgets, routing and the console ·
[Hardening](https://github.com/vul-os/llmux/blob/main/HARDENING.md) before you
expose it.

## When it does not work

The most common four, with the one-line answer:

| Symptom | Usually |
|---|---|
| `403` with a sovereignty message | The provider is off-box and you have not opted it in. That is the gate working — see [the sovereignty gate](architecture.md#the-sovereignty-gate-where-your-ai-runs) |
| `402 budget_exceeded` on a key that has barely spent anything | A leaked in-flight reservation — an embedder that did not call `release` |
| `400` refusing to serve a model on a budgeted key | The catalog cannot price that model, so the spend would go uncounted. Add a price override |
| The package spawns nothing and hangs, or errors on resolution | No binary found — set `LLMUX_BINARY` |
| Direct mode cannot find the shared library | Build it with `scripts/build-ffi.sh` and set `LLMUX_LIBRARY` (or `LLMUX_LIB` on Node/Bun/Deno). There is no library at all for Windows or Intel macOS — use the sidecar there |
| A forked worker passes its health check and then hangs on the first chat | The Go runtime is not fork-safe, and `models` is answered from memory so it **succeeds in a broken child**. Load the library after the fork, in the worker — see [Troubleshooting](TROUBLESHOOTING.md#embedded-and-c-abi-hosts) |

→ [Troubleshooting](TROUBLESHOOTING.md) is symptom-by-symptom and much longer.

## Related

- [Choosing a mode](choosing-a-mode.md) — the decision, with the trade-offs
- [Getting started](GETTING-STARTED.md) — the full operator path
- [Client examples](client-examples.md) — every language, plain HTTP
- [Embedding llmux](embedding.md) · [The C ABI](c-abi.md) · [Language packages](sdks.md)
