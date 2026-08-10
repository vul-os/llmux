# llmux (Ruby)

Two ways to use llmux from Ruby, both supported:

| mode | what it is | file |
| --- | --- | --- |
| **Sidecar** | the gem spawns `llmux serve` on a loopback port, waits for it to be healthy, and shuts it down at exit | `lib/llmux.rb` |
| **Direct** | `libllmux` loaded into the Ruby process with `fiddle` — no child process, no port | `lib/llmux/ffi.rb` |

**Which one to pick.** Unlike PHP, Ruby's FFI story here is genuinely good:
`fiddle` is in the standard library, its closures reacquire the GVL correctly,
and everything below was measured rather than assumed. So the answer depends on
one thing only — **does your process fork?**

| your process | pick |
| --- | --- |
| Unicorn; Puma in **clustered** mode (`workers > 0`); Passenger; Resque; Spring; anything with `preload_app` | **sidecar** |
| Puma in **single** mode, Falcon, Sidekiq, rake tasks, CLI tools, one-shot scripts, daemons | either — direct is fine and is one less process |
| you are deploying to **linux/amd64** | **sidecar**, today — see [platforms](#platforms) |

## Sidecar

```ruby
require "llmux"

base = Llmux.base_url          # => "http://127.0.0.1:<port>" — starts it on first use
v1   = Llmux.openai_base_url   # => "http://127.0.0.1:<port>/v1"

# Or a configured ruby-openai client (optional `ruby-openai` gem):
client = Llmux.openai
resp = client.chat(parameters: {
  model: "anthropic/claude-3-5-sonnet",
  messages: [{ role: "user", content: "hi" }],
})
```

The sidecar starts lazily on first use, is reused (singleton), and is terminated
at process exit. Runnable end to end, including SSE streaming and the error path:

```sh
ruby sdks/ruby/examples/sidecar_chat.rb
```

### Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `bin/llmux` (`bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the gem's `bin/`:

```sh
go build -o sdks/ruby/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

### Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

---

## Direct

```ruby
require "llmux/ffi"

Llmux::Ffi.open do |llmux|                # closes the handle however the block exits
  answer = llmux.call("chat",
                      model: "openai/gpt-4o-mini",
                      messages: [{ role: "user", content: "hi" }])
  puts answer.dig("choices", 0, "message", "content")

  llmux.stream("chat", model: "openai/gpt-4o-mini",
                       messages: [{ role: "user", content: "hi" }]) do |chunk, _raw|
    print chunk.dig("choices", 0, "delta", "content").to_s
  end
end
```

Requests and responses are the **same JSON the HTTP API uses**, so moving a call
site between the two modes is a transport change, not a rewrite. Return `:stop`
(or `false`) from a stream block to end it early — that is not an error, and
(measured, see [Cancellation](#cancellation)) it already stops the provider
too, not just delivery to your block.

```sh
ruby sdks/ruby/examples/direct_chat.rb
```

### `fiddle`, not the `ffi` gem

`fiddle` ships with Ruby. Adding `ffi` would put a native-extension gem into the
dependency graph of a gem whose entire pitch is that it is thin, and it would
buy nothing: fiddle has `dlopen`, the calling convention, and
`Fiddle::Closure::BlockCaller` for the streaming callback. It is a default gem,
so `gem "fiddle"` in a Gemfile is only needed if you want to pin it.

Library resolution is `LLMUX_LIBRARY`, then `sdks/ruby/lib/`, then
`dist/ffi/<goos>_<goarch>/` in a checkout, then the bare soname. Build one with
`scripts/build-ffi.sh`.

### The GVL, measured

The header says the chunk callback runs on the thread that called
`llmux_stream`, synchronously. That claim is asserted in `ffi/ctest/smoke.c` with
`pthread_self()`, and it holds from Ruby too — the block sees
`Thread.current == ` the caller's thread. So the interesting question is not
which thread but **who holds the GVL**, and the answer needed checking rather
than guessing, because fiddle's default is the surprising one:

- `Fiddle::Function#call` **releases the GVL by default** — `need_gvl:` defaults
  to `false`, and `ext/fiddle/function.c` then routes the call through
  `rb_thread_call_without_gvl`.
- `Fiddle::Closure` **reacquires it** — `ext/fiddle/closure.c` does
  `if (ruby_thread_has_gvl_p()) … else rb_thread_call_with_gvl(...)` before
  invoking your block. That is valid precisely because the callback arrives on
  the calling thread, which is a registered Ruby thread.

So the two facts compose, and `lib/llmux/ffi.rb` leaves `need_gvl` alone. What
that is worth, measured on this machine (50 streamed completions against a fake
upstream, with one background Ruby thread doing string work):

| `need_gvl` | chunks delivered | background thread iterations |
| --- | --- | --- |
| `false` (the default, what the SDK uses) | 300 | **890,224** |
| `true` | 300 | **0** |

Both are correct; only one lets the rest of your application run during a call
that takes hundreds of milliseconds. `examples/direct_chat.rb` prints the same
number for a single call.

**Do not raise out of a stream block.** An exception raised inside a
`Fiddle::Closure` unwinds by `longjmp` straight through the Go call frame. It
does not crash — measured — but llmux's own cleanup for that stream never runs.
`Ffi#stream` therefore catches everything, returns non-zero to stop the stream
politely, and re-raises once the Go frame has unwound. You get your exception;
llmux gets to clean up.

### Cancellation

llmux 0.1.5 added a seventh ABI function, `llmux_cancel(h)`, alongside the six
`Ffi` already bound (`llmux_abi_version`, `llmux_new`, `llmux_close`,
`llmux_call`, `llmux_free`, `llmux_stream`). It aborts every call in flight on
a handle without closing the handle itself — the gateway stays open, and the
next call starts on a fresh context. Before it, the only way out of a blocked
call was `close`, which tears down the gateway and every other stream running
on it.

**Ruby has real threads, and the GVL question was measured, not assumed: a
second thread CAN cancel a blocked stream.** `Fiddle::Function#call` releases
the GVL by default (see [The GVL, measured](#the-gvl-measured) above), so a
call blocked in `llmux_stream` does not stop another thread from calling
`llmux.cancel`. Measured against `sdks/fake-upstream.py` (100 ms/chunk, ten
words), cancelling from a second thread after the streaming thread had
delivered exactly 3 chunks:

```
rc=-1 err="context canceled"
consumer chunk count=3
cancel-call-to-thread-join elapsed=0.0015s
```

and the upstream's own count, queried at `GET /generated` after the fact —
the number that matters, because it is what the provider actually produced and
metered, not what your callback happened to see — read **3**, against **12**
for a run left to finish (ten word chunks, one finish chunk, one usage chunk;
the JS runtimes' harness has no usage frame, so their equivalent full count is
11 — see `sdks/fake-upstream.py`'s own docstring).

**The idiomatic construct is `#stream_enum`, a real `Enumerator`, because
`break` on it is safe in a way `break` inside `#stream`'s own block is not.**
`#stream` calls your block directly from inside the FFI trampoline that Go
invokes per chunk, so a bare `break` there would have to unwind past that
trampoline and the blocked `llmux_stream` C frame to reach `#stream`'s call
site — the same family of hazard as raising out of the block, above, and not
one this SDK tries to make safe. Use `#stream_enum` instead:

```ruby
llmux.stream_enum("chat", model: "…", messages: [...]).each do |chunk, _raw|
  print chunk.dig("choices", 0, "delta", "content")
  break if enough?
end
```

`stream_enum` wraps `#stream` in `Enumerator.new`, and the `break` above targets
`each`, not `stream` — the block you write runs inside `Enumerator::Yielder#<<`,
one level further out than the FFI trampoline, not inside it. `stream_enum`'s
`ensure` calls `cancel` unconditionally (a no-op on a stream that already ran to
completion). Measured on Ruby 4.0.5: breaking after 3 chunks ran that `ensure`
**immediately** — not on garbage collection, right there as `each` unwound —
and the upstream's `/generated` read exactly **3**, matching the 3-vs-12 numbers
above. `examples/cancel_demo.rb` runs this end to end and prints both numbers.

**`llmux.cancel` is also safe to call from inside a stream block**, on the same
thread that is blocked in `llmux_stream` — that path is what a host with no
threads at all (PHP, Node's main thread) has to rely on instead, and it was
verified safe there before this ABI function shipped. In Ruby it is mostly
useful when you want `stream`'s call to come back as the `context canceled`
error rather than a quiet `:stop`.

**`llmux_cancel` is per HANDLE, not per call.** It aborts every call in flight
on that gateway, not just the one you meant to stop. If you need independent
cancellation for concurrent streams, give each its own `Llmux::Ffi` handle.

**`Thread#kill` does not stop generation — measured, and it is the opposite of
safe to assume otherwise.** Killing the thread running a blocked `stream` call
(no `cancel`, just `Thread#kill`) does make the `Thread` object die — `alive?`
turns false, `join` returns — but the native call underneath keeps running
disconnected from any Ruby code that could observe or await it:

```
thread dead? true  consumer saw 3 chunks at kill time
t+0.5s: generated=8
t+1.0s: generated=12     <- ran to completion in the background, unread
t+1.5s: generated=12
```

`Thread#kill` cannot preempt a blocked native call any more than a `pcntl`
signal can in PHP — Ruby can only deliver the kill at a safe point, and there
is no such point inside `libllmux`'s blocked read. What actually happened here
is worse than a hang: the thread *looks* gone while the provider keeps
generating and metering on your behalf, silently, for as long as the response
takes. If you must be able to abandon a streaming thread, call `cancel` before
(or instead of) killing it — never rely on `kill` alone to stop the provider.

### It is not fork-safe

After `fork()` without `exec()`, the Go runtime in the child is broken: its
threads did not come across. `examples/fork_probe.rb` reproduces it. On Ruby
4.0.5 (arm64-darwin24) against llmux 0.1.2:

```
$ ruby sdks/ruby/examples/fork_probe.rb before chat
load=before method=chat   -> child HUNG (SIGKILLed after 10s)

$ ruby sdks/ruby/examples/fork_probe.rb after chat
load=after  method=chat   -> child exited 0

$ ruby sdks/ruby/examples/fork_probe.rb before models
load=before method=models -> child exited 0        <-- the trap
```

`before` means loaded before the fork, which is what `preload_app true` and
Puma's `workers`/`preload_app!` do for you.

**The third line is the dangerous one.** `models` is answered from memory and
never touches the Go netpoller, so it succeeds in a child whose runtime is
already broken. A boot check built on it says everything is fine. `chat` opens a
socket and hangs forever. Whether the bug shows up depends on which method you
happened to call.

Concrete victims in Ruby, and the fix in each:

| host | why | fix |
| --- | --- | --- |
| **Unicorn** | always pre-forks; `preload_app true` loads before the fork | build the `Ffi` in `after_fork`, never at boot |
| **Puma clustered** (`workers 2+`) | `preload_app!` loads before the fork | build it in `on_worker_boot`, or use `preload_app!` off |
| **Passenger** (smart spawning) | forks a preloader | `passenger_spawn_method direct`, or build it per worker |
| **Resque** | forks per job | build it in the child, or use the sidecar |
| **Spring** | forks the preloaded app for each command | use the sidecar in development |
| **`Process.fork` in your own code** | same thing | load after the fork |
| Puma **single** mode, Falcon, Sidekiq, rake, CLI | do not fork | nothing to do |

`exec`-based supervisors are fine, because `exec` replaces the image.

### Platforms

Prebuilt `libllmux` exists for **darwin/arm64** (12,823,104 bytes, C smoke test
40/40) and **linux/arm64** (17,356,264 bytes, same test in a `golang:1.25`
container). **linux/amd64** is built in CI only and has never been produced on a
developer machine here. **windows/amd64 and darwin/amd64 do not exist — no
`.dll` has ever been built by anyone.**

Most Ruby is deployed on linux/amd64, which is exactly the row with no locally
built and tested library today. The `llmux` binary the sidecar spawns has no
such gap. That is a second, independent reason to default to the sidecar for a
web deployment even though Ruby's FFI itself is in good shape.

### The rest of the honest list

1. **The Go runtime lives in your process** — its GC, its scheduler, and its
   signal handlers. Measured: five are replaced (`SIGSEGV`, `SIGBUS`, `SIGFPE`,
   `SIGPIPE`, `SIGURG`) and three more keep their handler but gain `SA_ONSTACK`
   (`SIGILL`, `SIGXFSZ`, `SIGUSR2`). Ruby installs its own, and Go chains to a
   pre-existing handler in most cases; "most" is the honest word.

   **`stackprof` is not the thing to watch.** `SIGPROF` — `stackprof`'s `:cpu`
   mode — was measured untouched, and `:wall` mode's `SIGALRM` is excluded by the
   same rule: under `-buildmode=c-shared` Go installs handlers only for
   synchronous signals plus `SIGPIPE` and `SIGURG`. The measurement is in [`sdks/java/README.md`](../java/README.md#the-jvm-and-gos-signal-handlers).
2. **Not fork-safe** — see above.
3. **The shared library is 12–17 MB**, loaded per process that uses it.
4. **Prebuilt binaries cover darwin/arm64 and linux/arm64 only** — see
   [Platforms](#platforms).
5. **Latency is not the reason to embed.** Measured: the boundary is ~4 µs
   in-process against ~46 µs over loopback, and a real chat call is ~80–92 µs
   against ~102–109 µs. Against a model answering in hundreds of milliseconds
   that is noise. The reasons are: no second process to supervise, no port to
   bind, no loopback surface to secure.
6. **No auth on the direct boundary, by design.** Virtual keys, budgets and
   per-key model allow-lists are enforced by the HTTP shell's middleware. An
   in-process host is already inside the trust boundary. If you need per-tenant
   keys, that is the sidecar's job.
7. **Construction is inert.** `Llmux::Ffi.new` starts no goroutines and opens no
   sockets, and there is no background price-catalog sync in library mode. The
   sidecar *does* sync prices in the background — which is why
   `examples/sidecar_chat.rb` reports several hundred models and
   `examples/direct_chat.rb` reports seven against the same config.

## Examples

| file | mode | what it shows |
| --- | --- | --- |
| `examples/sidecar_chat.rb` | sidecar | spawn, models, chat, SSE stream, HTTP error, guaranteed stop |
| `examples/direct_chat.rb` | direct | version probe, models, chat, callback stream, early stop, error path, GVL measurement, `Ffi.open` |
| `examples/fork_probe.rb` | direct | reproduces the fork hazard, and the false green that hides it |

All three run against a fake upstream with no provider key:

```sh
go build -o /tmp/fakeupstream ./ffi/fakeupstream
/tmp/fakeupstream -text "alpha bravo charlie delta" &
CFG=$(…the CONFIG line it printed…)

LLMUX_CONFIG_JSON="$CFG" LLMUX_MODEL=demo ruby sdks/ruby/examples/direct_chat.rb
echo "$CFG" > /tmp/llmux.json
LLMUX_CONFIG=/tmp/llmux.json LLMUX_MODEL=demo ruby sdks/ruby/examples/sidecar_chat.rb
```
