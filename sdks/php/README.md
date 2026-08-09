# llmux (PHP)

Two ways to use llmux from PHP, both supported:

| mode | what it is | file |
| --- | --- | --- |
| **Sidecar** *(recommended for PHP)* | the SDK spawns `llmux serve` on a loopback port, waits for it to be healthy, and shuts it down at exit | `src/Llmux.php` |
| **Direct** | `libllmux` loaded into the PHP process through ext-ffi — no child process, no port | `src/Ffi.php` |

**For PHP the sidecar is the right default, and this is not a hedge.** The
measurements are in [Direct mode, and why not to use it under php-fpm](#direct-mode-and-why-not-to-use-it-under-php-fpm)
below; the short version is that php-fpm forks its workers and the Go runtime
inside `libllmux` does not survive `fork()`. This is the same call
[`patala-go`](https://github.com/vul-os/patala) documents for cackle: naming the
mode that fits is part of the job.

## Sidecar

```php
use Llmux\Llmux;

$base = Llmux::baseUrl();        // http://127.0.0.1:<port> — starts it on first use
$v1   = Llmux::openaiBaseUrl();  // http://127.0.0.1:<port>/v1

// Or a configured openai-php client (optional openai-php/client):
$client = Llmux::openai();
$r = $client->chat()->create([
    'model'    => 'anthropic/claude-3-5-sonnet',
    'messages' => [['role' => 'user', 'content' => 'hi']],
]);
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
at process shutdown. Runnable end to end, including SSE streaming and the error
path:

```sh
php sdks/php/examples/sidecar_chat.php
```

### Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `bin/`:

```sh
go build -o sdks/php/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

### Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

---

## Direct mode, and why not to use it under php-fpm

```php
use Llmux\Ffi;

$llmux = new Ffi();              // null config = defaults + environment
try {
    $answer = $llmux->call('chat', [
        'model'    => 'openai/gpt-4o-mini',
        'messages' => [['role' => 'user', 'content' => 'hi']],
    ]);
    echo $answer['choices'][0]['message']['content'];
} finally {
    $llmux->close();             // PHP has no `using`; try/finally is the idiom
}
```

`Ffi::with($config, fn ($llmux) => …)` wraps the same try/finally if you prefer
a closure. Requests and responses are the **same JSON the HTTP API uses**, so
switching a call site between the two modes is a transport change, not a rewrite.

```sh
php sdks/php/examples/direct_chat.php
```

### You must enable the FFI extension

`ext-ffi` ships with PHP 7.4+ but is gated by the `ffi.enable` php.ini
directive, whose **default value is `preload`**:

| `ffi.enable` | CLI (`php script.php`) | php-fpm / mod_php / any other SAPI |
| --- | --- | --- |
| `preload` *(default)* | FFI works — CLI is exempt | `FFI\Exception: FFI API is restricted by "ffi.enable" configuration directive` |
| `false` | blocked everywhere | blocked everywhere |
| `true` | works | works — and PHP can now dlopen and call **any** native library |

Observed on PHP 8.5.9 (Homebrew, darwin/arm64), using the built-in server as a
stand-in for a non-CLI SAPI:

```
$ php -d ffi.enable=0 -r 'FFI::cdef("int puts(const char*);", null);'
FFI BLOCKED: FFI\Exception: FFI API is restricted by "ffi.enable" configuration directive

$ php -S 127.0.0.1:19182 ffigate.php          # ffi.enable=preload, the default
FFI BLOCKED: FFI\Exception: FFI API is restricted by "ffi.enable" configuration directive

$ php -d ffi.enable=1 -S 127.0.0.1:19183 ffigate.php
FFI OK
```

So a web deployment needs `ffi.enable=1` in `php.ini` (a global, unrestricted
native-code capability for every script the interpreter runs) or an
`opcache.preload` script — and the preload route has its own problem, below.
Check what you have:

```sh
php -r 'var_dump(extension_loaded("FFI"), ini_get("ffi.enable"));'
```

Also required: a `libllmux` for your platform. Resolution order is
`LLMUX_LIBRARY`, then `sdks/php/lib/`, then `dist/ffi/<goos>_<goarch>/` in a
checkout, then the bare soname. Build one with `scripts/build-ffi.sh`.

### It is not fork-safe, and php-fpm forks

After `fork()` without `exec()`, the Go runtime in the child is broken: its
threads did not come across. `examples/fork_probe.php` reproduces it. On PHP
8.5.9 / darwin/arm64 against llmux 0.1.2:

```
$ php sdks/php/examples/fork_probe.php before chat
load=before method=chat   -> child HUNG (SIGKILLed after 10s)

$ php sdks/php/examples/fork_probe.php after chat
load=after  method=chat   -> child exited 0

$ php sdks/php/examples/fork_probe.php before models
load=before method=models -> child exited 0        <-- the trap
```

`before` means the library was loaded before `pcntl_fork()`, which is what
php-fpm's master does for you. Five consecutive `before chat` runs hung five
times out of five.

**The third line is the dangerous one.** `models` is answered from memory and
needs nothing from the Go scheduler or netpoller, so it succeeds in a child
whose runtime is already broken (3/3 runs). A health check built on it reports
green. `chat` opens a socket and hangs forever. Whether your fork-unsafety shows
up depends on which method you happened to call, which is the worst possible
failure mode.

The same thing in real php-fpm, master + 2 static workers behind Caddy's
`php_fastcgi`, `libllmux` dlopen'ed in the master via `opcache.preload` +
`FFI::load()`:

| what the worker did | result |
| --- | --- |
| `FFI::scope('LLMUX')`, `llmux_new`, `llmux_call("models")` | `ok 1580 bytes in 0.1 ms` |
| same worker, `llmux_call("chat")` | **no response; curl gave up at 30 s (exit 28)** |
| no preload — `FFI::cdef()` in the worker itself, then `chat` | `ok 279 bytes in ~1–2 ms`, 3 requests, 3 successes |

One more thing that surfaced while building that: with the default
`ffi.enable=preload`, even the documented `FFI::load()` in preload +
`FFI::scope()` in the request is refused at request time on PHP 8.5.9 —
`FFI\Exception: FFI API is restricted`. Getting the preload route to work at all
needed `ffi.enable=1`, at which point it hangs on the first real call. **Under
php-fpm the only shape that works is `FFI::cdef()` inside the worker, after the
fork, with `ffi.enable=1` globally.**

That is: give every PHP script on the box the ability to call arbitrary native
code, re-`dlopen` a 12.7 MB library per worker, and be careful never to let it
load in the master. Against `Llmux::start()`, which needs no php.ini change and
no care at all.

### When direct mode *is* right for PHP

- Long-lived CLI processes: workers, queue consumers, cron jobs, `artisan`
  commands, one-shot scripts. No fork, no SAPI restriction, and the library is
  loaded exactly once by the process that uses it.
- Anywhere you specifically do not want a second process or a listening port —
  which is the actual reason to embed. It is **not** latency: measured, the
  boundary is ~4 µs in-process against ~46 µs over loopback, and a real chat
  call is ~80–92 µs against ~102–109 µs. Both are noise beside a model that
  answers in hundreds of milliseconds.

### The rest of the honest list

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers (`SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPROF`, …). PHP itself is
   quiet about signals, but extensions are not: Xdebug, `pcntl` signal
   dispatch, and APM agents all interact with the same handlers.
2. **Not fork-safe.** For PHP the concrete victims are **php-fpm** (any `pm`
   mode — workers are always forked from the master), **mod_php** under Apache
   `prefork`/`event` MPM, **`pcntl_fork()`** in your own code, and Swoole /
   RoadRunner / FrankenPHP worker pools that fork. `exec()`-based process
   managers are fine, because `exec` replaces the image.
3. **The shared library is 12–17 MB** — 12,787,504 bytes on darwin/arm64,
   17,348,392 bytes on linux/arm64. Per worker that loads it.
4. **Prebuilt libllmux binaries exist for darwin/arm64 and linux/arm64 only.**
   linux/amd64 is built in CI and has never been produced on a developer
   machine here; **windows/amd64 and darwin/amd64 do not exist — no `.dll` has
   ever been built.** Note that linux/amd64 is where most PHP is deployed, so
   for the typical PHP host there is no tested prebuilt library today. The
   sidecar binary has no such gap.
5. **No auth on the direct boundary, by design.** Virtual keys, budgets and
   per-key model allow-lists are enforced by the HTTP shell's middleware. An
   in-process host is already inside the trust boundary. If you need per-tenant
   keys, that is the sidecar's job.

### PHP-specific FFI behaviour worth knowing

Learned while writing `src/Ffi.php`, all of it observable on PHP 8.5.9:

- A `const char*` **return** (`llmux_abi_version`) arrives as a PHP `string`. A
  non-const `char*` return (`llmux_call`) arrives as `FFI\CData` — or as PHP
  `null` when the C function returned `NULL`. So the failure test is
  `$res === null`, not `FFI::isNull($res)`, which throws a `TypeError` on null.
- A `const char*` **parameter** into a PHP callback also arrives as a PHP
  string, already copied. The header warns that `chunk_json` is only valid for
  the duration of the callback; PHP has copied it for you before your code runs.
- **Throwing out of an FFI callback is a fatal error**, not a catchable one:
  `Fatal error: Throwing from FFI callbacks is not allowed`. `Ffi::stream()`
  therefore catches everything inside the trampoline, returns non-zero to stop
  the stream, and rethrows once the Go call frame has unwound.
- `llmux` writes `*err` **on failure only**, so `Ffi` allocates a fresh
  zero-initialised `char*` slot per call. Reusing one leaves a dangling pointer
  from the previous error, and reading it prints garbage.
- Every non-const `char*` the library returns — results **and** error messages —
  is released with `llmux_free` and nothing else. `Ffi` does that on the error
  path too, before it throws.
- `FFI::new()` called statically is deprecated in PHP 8.5; use
  `$ffi->new('char*')`.
- PHP's FFI parser does not run the C preprocessor, so `ffi/include/llmux.h`
  cannot be handed to `FFI::cdef` directly. The declarations are transcribed
  into `Ffi::CDEF`; if the header changes, that constant changes with it.

## Examples

| file | mode | what it shows |
| --- | --- | --- |
| `examples/sidecar_chat.php` | sidecar | spawn, models, chat, SSE stream, HTTP error, guaranteed stop |
| `examples/direct_chat.php` | direct | version probe, models, chat, callback stream, early stop, error path, `finally { close() }` |
| `examples/fork_probe.php` | direct | reproduces the fork hazard, and the false green that hides it |

All three run against a fake upstream with no provider key:

```sh
go build -o /tmp/fakeupstream ./ffi/fakeupstream
/tmp/fakeupstream -text "alpha bravo charlie delta" &
CFG=$(…the CONFIG line it printed…)

LLMUX_CONFIG_JSON="$CFG" LLMUX_MODEL=demo php sdks/php/examples/direct_chat.php
echo "$CFG" > /tmp/llmux.json
LLMUX_CONFIG=/tmp/llmux.json LLMUX_MODEL=demo php sdks/php/examples/sidecar_chat.php
```
