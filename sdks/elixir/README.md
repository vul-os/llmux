# llmux (Elixir)

Use llmux **locally** from Elixir. The package bundles the gateway binary,
starts it on a local port via an Erlang `Port` (managed by a singleton
GenServer, `Llmux.Sidecar`), and hands your OpenAI-compatible client a
`base_url`.

```elixir
{:ok, base} = Llmux.base_url()         # "http://127.0.0.1:<port>"
{:ok, v1} = Llmux.openai_base_url()    # "http://127.0.0.1:<port>/v1"
```

The sidecar starts lazily on first use, is reused (one GenServer process), and
is terminated when the GenServer stops — including BEAM shutdown, which closes
the Port and reaps the child.

> Why Port and not `System.cmd`: `System.cmd/3` blocks until the process exits,
> which is wrong for a long-lived sidecar. A `Port` gives us a non-blocking,
> supervised handle with exit notifications and automatic teardown — the
> idiomatic fit for the contract.

Two runnable examples:

```sh
cd sdks/elixir
mix run examples/sidecar_chat.exs     # models, chat, error path, crash isolation
mix run examples/sidecar_stream.exs   # SSE streaming, early stop, killing a consumer
```

## Binary resolution

1. `LLMUX_BINARY` env var
2. bundled `priv/bin/llmux` (`priv/bin/llmux.exe` on Windows)
3. `llmux` on `PATH`

For local development, build it into the package's `priv/bin/`:

```sh
go build -o sdks/elixir/priv/bin/llmux ./cmd/llmux
# or: make sdk-bins
```

## Provider keys

Provider API keys are inherited from the environment (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

---

## There is no direct (in-process) mode for Elixir, and that is the recommendation

llmux ships a C ABI (`ffi/include/llmux.h`) so that hosts can run the gateway
inside their own process. Every other SDK in `sdks/` binds it. **Elixir does
not, deliberately.** This section is the reasoning, not an apology — if it ever
stops holding, the ABI is right there and the door is open.

### There is no FFI in OTP; the choice is between four things

Erlang/OTP has no `ctypes`, no `fiddle`, no `FFI::cdef`. To call
`llmux_call()` from Elixir the options are:

| mechanism | what it costs | verdict |
| --- | --- | --- |
| **NIF** | C you write, compile and ship per platform; runs on a scheduler thread; a crash or a hang takes the whole VM | the only true in-process option, and the one this section rejects |
| **Linked-in port driver** (`:erl_ddll`) | same C, same address space, same blast radius, an older API | strictly worse than a NIF |
| **Port to a C program that dlopens libllmux** | safe, but it is a second OS process — the exact thing in-process mode exists to avoid — and now you maintain a C program too | worse than the sidecar in every dimension |
| **Managed sidecar** (`Llmux.Sidecar`) | a second OS process, supervised by a `Port`, reaped by the VM | **what this package does** |

The third row is the one worth sitting with. The whole argument for the C ABI
is *no second process, no port, no loopback surface*. Every safe way to reach it
from the BEAM reintroduces a second process. So the choice is really "a NIF" or
"the sidecar", and the NIF has to earn it.

### Why a NIF does not earn it

**1. The scheduler budget is ~1 ms, and llmux calls are not.** ERTS expects a
NIF to return within about a millisecond so it does not stall a scheduler
thread. Measured, one in-process `chat` through the C ABI is ~80–92 µs against a
*fake* upstream (`ffi/README.md`) — comfortably inside the budget, and
completely unrepresentative. A real completion takes hundreds of milliseconds to
tens of seconds. That is 100× to 10,000× over budget, so a plain NIF is not on
the table: it would have to be a dirty NIF.

**2. Dirty schedulers are a small, fixed pool.** On this machine
(`erlang:system_info/1`, OTP 29, 8 cores): 8 normal schedulers, 8 dirty-CPU, and
**10 dirty-IO**. A dirty-IO NIF blocks one of those ten for the entire duration
of the model call. Eleven concurrent conversations and the eleventh waits — in a
runtime whose defining property is that you can have a million processes waiting
on a million things at once. The sidecar has no such ceiling: each request is a
process doing ordinary socket I/O, and the VM handles hundreds of thousands of
those.

**3. A crash stops being local.** "Let it crash" works because a process dying
is contained. A segfault inside a NIF is not a process dying; it is the VM
dying, taking every unrelated supervision tree with it. `examples/sidecar_chat.exs`
kills a worker mid-flight and prints that the gateway is still answering;
`examples/sidecar_stream.exs` kills a consumer mid-stream and prints the same.
Neither of those is a property a NIF can offer.

**4. A NIF cannot be preempted, killed, or timed out.** The BEAM's scheduler is
preemptive by reduction counting; native code inside a NIF is not. A hung
upstream inside a NIF is a hung scheduler thread that `Process.exit(pid, :kill)`
and `Task.await/2` timeouts cannot touch. Over the sidecar, a slow request is a
process blocked on a socket — cancellable, supervisable, and observable.

**5. Two runtimes in one address space.** The BEAM has a preemptive scheduler
and per-process GC; the Go runtime has its own scheduler, a global GC, and
signal handlers for `SIGSEGV`, `SIGBUS`, `SIGFPE`, `SIGPROF` and `SIGURG` (which
Go sends to its own threads constantly for asynchronous preemption). ERTS
installs its own handlers, including for crash dumps. Both are well-behaved
alone. Together they are a support burden no one has signed up for, and a
12.7 MB library mapped into the VM to boot.

**6. The streaming callback is the worst case, not the best.** `llmux_stream`
invokes a callback on the calling thread once per token. In a NIF that is a
scheduler thread running native code for the whole completion, calling
`enif_send` per token. Over SSE the same chunk objects arrive as ordinary
messages to an ordinary process — `examples/sidecar_stream.exs` shows the
result, including cancelling mid-stream by returning `:stop`.

**7. Nobody has to write, build, sign and ship C for eight platforms.** A NIF
would need a compiler on the user's machine or a precompiled artifact per
target, and today `libllmux` itself exists for **darwin/arm64** (12,787,504
bytes) and **linux/arm64** (17,348,392 bytes) only; linux/amd64 is built in CI
and never locally, and **windows/amd64 and darwin/amd64 have never been built at
all**. Most BEAM deployments are linux/amd64. The `llmux` binary the sidecar
spawns has no such gap.

**8. There is no latency argument.** Measured: the ABI boundary is ~4 µs
in-process against ~46 µs over loopback, and a real chat call is ~80–92 µs
against ~102–109 µs. Both are noise beside a model that answers in hundreds of
milliseconds. Nobody should trade the BEAM's isolation guarantees for 40 µs.

### If you genuinely need llmux in-process

Two honest answers:

- **Write the Go host in Go.** `github.com/vul-os/llmux/core/gateway` is a
  library; a Go service imports it directly with no C ABI in the middle. If the
  in-process requirement is real, that is the shape that satisfies it.
- **Run the sidecar on the same box, on loopback or a unix socket.** You keep
  process isolation, independent restarts, and the ability to upgrade llmux
  without recompiling your release. `Llmux.Sidecar` already supervises it.

If you do build a NIF anyway, do it in your own application rather than here, use
`ERL_NIF_DIRTY_JOB_IO_BOUND`, never load the library in a process that will
`fork()`, and read `ffi/README.md` first — the fork-unsafety and signal-handling
notes there apply to you in full.

## Honest notes that apply to the sidecar too

1. **It is a second OS process.** ~19 MB binary, its own memory, its own
   lifecycle. The `Port` reaps it when the GenServer stops or the VM shuts down;
   a `kill -9` of the VM leaves that to the OS.
2. **It listens on a loopback port.** `Llmux.Sidecar` picks a free one, but it
   is a socket on the box, and anything running as any user on that box can
   reach it. Set virtual keys in the config if that matters.
3. **It syncs the price catalogue in the background** and the library mode does
   not. That is why `examples/sidecar_chat.exs` reports several hundred models
   against a config that defines one route.
4. **`llmux serve` is where authentication lives.** Virtual keys, budgets and
   per-key model allow-lists are enforced by its HTTP middleware. For Elixir
   that is a feature of the mode you are already using, not a thing you gave up.
