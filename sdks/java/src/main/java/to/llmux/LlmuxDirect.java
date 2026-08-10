package to.llmux;

import java.lang.foreign.Arena;
import java.lang.foreign.FunctionDescriptor;
import java.lang.foreign.Linker;
import java.lang.foreign.MemorySegment;
import java.lang.foreign.SymbolLookup;
import java.lang.foreign.ValueLayout;
import java.lang.invoke.MethodHandle;
import java.lang.invoke.MethodHandles;
import java.lang.invoke.MethodType;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

/**
 * llmux <b>in this process</b>, through the C ABI of {@code libllmux}.
 *
 * <p>This is the direct path. {@link Llmux} is the sidecar path — it spawns
 * {@code llmux serve} and talks HTTP to it. <b>For the JVM the sidecar is the
 * recommended default</b>; see {@code README.md} → "The JVM and Go's signal
 * handlers" for the measurements behind that recommendation. This class exists
 * because the direct path is real, works, and is the right answer for some
 * hosts — not because it is the one to reach for first.
 *
 * <p>Requests and responses are JSON: the same JSON the HTTP API uses. This
 * class deliberately does not parse it. Hand the strings to whatever JSON
 * library you already have.
 *
 * <pre>{@code
 * try (LlmuxDirect llmux = LlmuxDirect.open(configJson)) {
 *     String models = llmux.call("models", null);
 *     String answer = llmux.call("chat", "{\"model\":\"demo\",\"messages\":[...]}");
 * }
 * }</pre>
 *
 * <h2>Requirements</h2>
 * <ul>
 *   <li><b>Java 22 or newer.</b> This class uses {@code java.lang.foreign} (the
 *       FFM API), which left preview in Java 22. Developed and tested on
 *       OpenJDK 26.0.2, darwin/arm64.</li>
 *   <li>The JVM must be started with {@code --enable-native-access=ALL-UNNAMED}
 *       (or the named module). Without it JDK 24+ prints a warning on the first
 *       restricted call, and a future release will throw.</li>
 *   <li>A {@code libllmux} for your platform. <b>darwin/arm64 and linux/arm64
 *       only today. There is no Windows DLL.</b> See the README.</li>
 * </ul>
 *
 * <h2>Threading</h2>
 * A handle is safe to use from several threads at once — that is a property of
 * the library, not of this wrapper. {@link #stream} runs its callback on the
 * calling thread, synchronously, before {@code stream} returns.
 *
 * <h2>Cancellation</h2>
 * Before llmux 0.1.5 the only way out of a blocked {@link #call} or
 * {@link #stream} was {@link #close()}, which destroys the handle and every
 * other call running on it. {@link #cancel()} aborts what is in flight
 * without closing anything, and interrupting the thread blocked in
 * {@link #stream} reaches it too — see that method's own "Cancellation"
 * section for the mechanism and {@link #cancel()}'s doc for the per-handle
 * caveat that applies to both paths.
 *
 * <h2>Lifetime</h2>
 * The shared library is loaded into {@link Arena#global()} and is never
 * unloaded: {@code dlclose} on a library carrying the Go runtime is not
 * something to attempt. Handles, by contrast, are yours to close, and
 * {@link #close()} is idempotent — use try-with-resources.
 */
public final class LlmuxDirect implements AutoCloseable {

    /** One chunk of a streaming response. */
    @FunctionalInterface
    public interface ChunkHandler {
        /**
         * @param chunkJson one {@code chat.completion.chunk} object, already
         *                  copied out of the library's memory
         * @return {@code true} to keep streaming, {@code false} to stop.
         *         Stopping is not an error.
         */
        boolean onChunk(String chunkJson);
    }

    // ---------------------------------------------------------------- binding

    /** The resolved entry points of one loaded copy of libllmux. */
    private static final class Native {
        final MethodHandle abiVersion;   // const char* (void)
        final MethodHandle newGateway;   // uint64_t (const char*, char**)
        final MethodHandle closeGateway; // void (uint64_t)
        final MethodHandle cancelGateway; // void (uint64_t)
        final MethodHandle call;         // char* (uint64_t, const char*, const char*, char**)
        final MethodHandle free;         // void (char*)
        final MethodHandle stream;       // int (uint64_t, const char*, const char*, cb, void*, char**)
        final FunctionDescriptor cbDesc; // int (const char*, void*)
        final String version;

        Native(Path library) {
            Linker linker = Linker.nativeLinker();
            SymbolLookup lookup;
            try {
                // Arena.global(): the library stays mapped for the life of the
                // process. Unloading a Go runtime is not a supported operation.
                lookup = SymbolLookup.libraryLookup(library, Arena.global());
            } catch (IllegalArgumentException e) {
                throw new LlmuxException("could not load " + library + ": " + e.getMessage(), e);
            }
            abiVersion = down(linker, lookup, "llmux_abi_version",
                    FunctionDescriptor.of(ValueLayout.ADDRESS));
            newGateway = down(linker, lookup, "llmux_new",
                    FunctionDescriptor.of(ValueLayout.JAVA_LONG, ValueLayout.ADDRESS, ValueLayout.ADDRESS));
            closeGateway = down(linker, lookup, "llmux_close",
                    FunctionDescriptor.ofVoid(ValueLayout.JAVA_LONG));
            cancelGateway = down(linker, lookup, "llmux_cancel",
                    FunctionDescriptor.ofVoid(ValueLayout.JAVA_LONG));
            call = down(linker, lookup, "llmux_call",
                    FunctionDescriptor.of(ValueLayout.ADDRESS, ValueLayout.JAVA_LONG,
                            ValueLayout.ADDRESS, ValueLayout.ADDRESS, ValueLayout.ADDRESS));
            free = down(linker, lookup, "llmux_free",
                    FunctionDescriptor.ofVoid(ValueLayout.ADDRESS));
            stream = down(linker, lookup, "llmux_stream",
                    FunctionDescriptor.of(ValueLayout.JAVA_INT, ValueLayout.JAVA_LONG,
                            ValueLayout.ADDRESS, ValueLayout.ADDRESS, ValueLayout.ADDRESS,
                            ValueLayout.ADDRESS, ValueLayout.ADDRESS));
            cbDesc = FunctionDescriptor.of(ValueLayout.JAVA_INT, ValueLayout.ADDRESS, ValueLayout.ADDRESS);

            try {
                MemorySegment v = (MemorySegment) abiVersion.invokeExact();
                // A static string owned by the library: read it, never free it.
                version = v.reinterpret(Long.MAX_VALUE).getString(0);
            } catch (Throwable t) {
                throw new LlmuxException("llmux_abi_version failed", t);
            }
        }

        private static MethodHandle down(Linker linker, SymbolLookup lookup, String name,
                                         FunctionDescriptor desc) {
            MemorySegment sym = lookup.find(name).orElseThrow(() -> new LlmuxException(
                    "libllmux does not export " + name
                            + " — the library on the load path is not a libllmux, or is too old"));
            return linker.downcallHandle(sym, desc);
        }
    }

    private static final Map<Path, Native> LOADED = new ConcurrentHashMap<>();

    private static Native nativeFor(Path library) {
        Path key = library.toAbsolutePath().normalize();
        return LOADED.computeIfAbsent(key, Native::new);
    }

    // The upcall trampoline. One MethodHandle, bound per stream call to the
    // handler for that call, so there is no shared mutable dispatch table.
    private static final MethodHandle TRAMPOLINE;
    static {
        try {
            TRAMPOLINE = MethodHandles.lookup().findStatic(LlmuxDirect.class, "trampoline",
                    MethodType.methodType(int.class, StreamState.class,
                            MemorySegment.class, MemorySegment.class));
        } catch (ReflectiveOperationException e) {
            throw new ExceptionInInitializerError(e);
        }
    }

    // ---------------------------------------------------------------- state

    private final Native lib;
    private final Path libraryPath;
    private volatile long handle;

    private LlmuxDirect(Native lib, Path libraryPath, long handle) {
        this.lib = lib;
        this.libraryPath = libraryPath;
        this.handle = handle;
    }

    // ---------------------------------------------------------------- opening

    /** Open a gateway using the library found by {@link #findLibrary()}. */
    public static LlmuxDirect open(String configJson) {
        return open(findLibrary(), configJson);
    }

    /**
     * Open a gateway from an explicit library path.
     *
     * @param library    path to libllmux.dylib / libllmux.so
     * @param configJson an llmux configuration document, or {@code null} for
     *                   built-in defaults plus the environment
     */
    public static LlmuxDirect open(Path library, String configJson) {
        Native lib = nativeFor(library);
        try (Arena arena = Arena.ofConfined()) {
            MemorySegment cfg = configJson == null
                    ? MemorySegment.NULL
                    : arena.allocateFrom(configJson);
            MemorySegment errBox = arena.allocate(ValueLayout.ADDRESS);
            errBox.set(ValueLayout.ADDRESS, 0, MemorySegment.NULL);

            long h;
            try {
                h = (long) lib.newGateway.invokeExact(cfg, errBox);
            } catch (Throwable t) {
                throw new LlmuxException("llmux_new failed", t);
            }
            if (h == 0) {
                throw new LlmuxException("llmux_new: " + takeError(lib, errBox));
            }
            // Drain any message the library set alongside a success, so it
            // cannot leak. In practice there is none; costing nothing to be sure.
            drainError(lib, errBox);
            return new LlmuxDirect(lib, library, h);
        }
    }

    /** The version of llmux this shared library was built from. */
    public String abiVersion() {
        return lib.version;
    }

    /** The library this gateway is running out of. */
    public Path libraryPath() {
        return libraryPath;
    }

    // ---------------------------------------------------------------- calling

    /**
     * One unary call. {@code method} is {@code "chat"}, {@code "embed"} or
     * {@code "models"}; {@code requestJson} may be null for {@code "models"}.
     *
     * @return the response JSON
     * @throws LlmuxException with the library's message on failure
     */
    public String call(String method, String requestJson) {
        long h = handle;
        if (h == 0) {
            throw new LlmuxException("this gateway is closed");
        }
        try (Arena arena = Arena.ofConfined()) {
            MemorySegment m = arena.allocateFrom(method);
            MemorySegment req = requestJson == null
                    ? MemorySegment.NULL
                    : arena.allocateFrom(requestJson);
            MemorySegment errBox = arena.allocate(ValueLayout.ADDRESS);
            errBox.set(ValueLayout.ADDRESS, 0, MemorySegment.NULL);

            MemorySegment out;
            try {
                out = (MemorySegment) lib.call.invokeExact(h, m, req, errBox);
            } catch (Throwable t) {
                throw new LlmuxException("llmux_call failed", t);
            }
            if (out.equals(MemorySegment.NULL)) {
                throw new LlmuxException("llmux_call(" + method + "): " + takeError(lib, errBox));
            }
            try {
                return out.reinterpret(Long.MAX_VALUE).getString(0);
            } finally {
                // Copied into a java.lang.String; the C allocation is ours to
                // release and the library's allocator is the only one that can.
                freeNative(lib, out);
                drainError(lib, errBox);
            }
        }
    }

    /**
     * Stream a chat completion, calling {@code handler} once per chunk on
     * <b>this</b> thread. Blocks until the stream ends.
     *
     * <p>A handler returning {@code false} stops the stream and is not an
     * error. A handler that throws stops the stream and the exception is
     * rethrown here, after the library has unwound — an exception escaping an
     * FFM upcall stub would otherwise terminate the JVM.
     *
     * <h2>Cancellation</h2>
     * There are two ways out of a stream that has not finished on its own,
     * and they answer different questions.
     *
     * <p><b>{@link #cancel()}</b> is the direct one: call it from any other
     * thread and this call fails promptly, however far into the stream it is.
     * That is what a controller thread that is watching the stream from
     * outside should reach for.
     *
     * <p><b>Interrupting this thread</b> is the idiomatic Java one, and it is
     * wired to reach the same place. A downcall into native code is not
     * interruptible the way {@code Thread.sleep} or a channel read is —
     * {@code Thread.interrupt()} only sets a flag, it does not unblock
     * {@code llmux_stream}. What makes interruption work here is that
     * {@code llmux_stream}'s callback runs on <b>this</b> thread once per
     * chunk (see the class doc's "Threading" section), which is the one place
     * this thread ever comes up for air while the call is blocked. So the
     * trampoline checks {@link Thread#interrupted()} there: if this thread was
     * interrupted since the last chunk, it calls {@link #cancel()} itself —
     * from inside the callback, which the C ABI documents as safe, unlike
     * {@link #close()} — and stops the stream. The result is
     * {@link InterruptedException}, not {@link LlmuxException}, with the
     * interrupt status left cleared exactly as {@code Thread.sleep} leaves it.
     * This is why {@code ExecutorService} + {@code Future} work as expected:
     * {@code Future.cancel(true)} does nothing but interrupt the thread
     * running the task, and that is enough to reach {@code llmux_cancel}
     * through this path. (What {@code future.get()} then throws is
     * {@code CancellationException}, per the {@code Future} contract — it does
     * not wait to see whether the task's own thread noticed the interrupt.)
     *
     * <p>One consequence of interruption being observed only between chunks:
     * if the stream has not delivered a first chunk yet — still waiting on the
     * network, still inside {@code stream_first_byte_timeout_seconds} — there
     * is no callback invocation for the trampoline to catch the flag on, and
     * interrupting does nothing until either a chunk arrives or llmux's own
     * timeout does. {@link #cancel()} has no such gap: it reaches the network
     * call directly and does not wait for a chunk.
     *
     * <p>Either path leaves the handle open: see {@link #cancel()}'s own doc
     * for the per-handle caveat that matters most, which applies here too.
     *
     * @return the number of chunks delivered
     * @throws InterruptedException if this thread was interrupted while the
     *         stream was in progress. The interrupt status is cleared, as
     *         {@code Thread.sleep} leaves it — this method does not restore
     *         it, because it is reporting the interruption directly rather
     *         than swallowing one it cannot propagate.
     */
    public int stream(String method, String requestJson, ChunkHandler handler) throws InterruptedException {
        long h = handle;
        if (h == 0) {
            throw new LlmuxException("this gateway is closed");
        }
        if (handler == null) {
            throw new IllegalArgumentException("handler is null");
        }
        StreamState state = new StreamState(handler, lib, h);

        try (Arena arena = Arena.ofConfined()) {
            MemorySegment cb = Linker.nativeLinker().upcallStub(
                    TRAMPOLINE.bindTo(state), lib.cbDesc, arena);
            MemorySegment m = arena.allocateFrom(method);
            MemorySegment req = requestJson == null
                    ? MemorySegment.NULL
                    : arena.allocateFrom(requestJson);
            MemorySegment errBox = arena.allocate(ValueLayout.ADDRESS);
            errBox.set(ValueLayout.ADDRESS, 0, MemorySegment.NULL);

            int rc;
            try {
                rc = (int) lib.stream.invokeExact(h, m, req, cb, MemorySegment.NULL, errBox);
            } catch (Throwable t) {
                throw new LlmuxException("llmux_stream failed", t);
            }
            if (state.interrupted) {
                // The trampoline saw this thread's interrupt flag, called
                // llmux_cancel itself, and told llmux_stream to stop. The C
                // ABI has no concept of a Java InterruptedException, so what
                // comes back here is an ordinary failure (rc=-1, "context
                // canceled") — free it and report the Java-idiomatic outcome
                // instead of wrapping it in LlmuxException.
                drainError(lib, errBox);
                throw new InterruptedException(
                        "llmux stream interrupted after " + state.chunks + " chunk(s)");
            }
            if (state.thrown != null) {
                drainError(lib, errBox);
                throw new LlmuxException("the chunk handler threw", state.thrown);
            }
            if (rc != 0) {
                throw new LlmuxException("llmux_stream(" + method + "): " + takeError(lib, errBox));
            }
            drainError(lib, errBox);
            return state.chunks;
        }
    }

    /**
     * Aborts every call in flight on this gateway — {@link #call} and
     * {@link #stream} alike — without closing it: the handle stays open and
     * the next call starts fresh. This is {@code llmux_cancel}; see
     * {@code ffi/include/llmux.h} for the contract it is documented against.
     *
     * <p>Safe from any thread, including a thread other than the one blocked
     * in {@link #call} or {@link #stream}, and safe to call from
     * <b>inside</b> a {@link ChunkHandler} running inside {@code this
     * object's} own {@link #stream} — unlike {@link #close()}, which must
     * never be called from inside a handler because it waits (up to a few
     * seconds) for the very call that is running the handler. A blocked
     * {@link #stream} that is cancelled this way returns by throwing
     * {@link LlmuxException} with the library's message ("context canceled"),
     * not {@link InterruptedException} — this method does not touch any
     * thread's interrupt status, so there is no interruption for
     * {@link #stream} to detect. To get {@link InterruptedException} instead,
     * interrupt the thread blocked in {@link #stream}; see that method's
     * "Cancellation" section, which this call is also what powers.
     *
     * <p>A no-op on a closed handle, on a handle with nothing running, and
     * safe to call twice — matching what {@code llmux_cancel} itself
     * guarantees for an unknown or idle handle.
     *
     * <p><b>This is per-HANDLE, not per-call.</b> It aborts every call
     * currently in flight on this gateway, not just the one you meant to
     * stop — a concurrent {@link #call} or a sibling {@link #stream} on the
     * very same {@code LlmuxDirect} is aborted too. If cancelling one logical
     * request must not touch another running on the same object, they need
     * separate gateways ({@link #open}), not separate calls on one.
     */
    public void cancel() {
        long h = handle;
        if (h == 0) {
            return; // closed; llmux_cancel on an unknown handle is a documented no-op anyway
        }
        try {
            lib.cancelGateway.invokeExact(h);
        } catch (Throwable t) {
            throw new LlmuxException("llmux_cancel failed", t);
        }
    }

    /** Per-stream state, reachable from the bound upcall and from nowhere else. */
    private static final class StreamState {
        final ChunkHandler handler;
        final Native lib;
        final long handle;
        int chunks;
        Throwable thrown;
        boolean interrupted;

        StreamState(ChunkHandler handler, Native lib, long handle) {
            this.handler = handler;
            this.lib = lib;
            this.handle = handle;
        }
    }

    @SuppressWarnings("unused") // invoked through TRAMPOLINE
    private static int trampoline(StreamState state, MemorySegment chunk, MemorySegment userData) {
        // NOTHING may propagate out of here. An exception crossing an upcall
        // stub takes the whole VM down, so a throwing handler is recorded and
        // turned into "stop the stream", which llmux_stream honours cleanly.
        try {
            if (state.thrown != null || state.interrupted) {
                return 1;
            }
            // Thread.interrupted() both reads AND CLEARS the flag, which is
            // exactly what Thread.sleep does before it throws
            // InterruptedException — this is the one point where the thread
            // blocked in llmux_stream ever runs Java code, so it is the only
            // place that flag can be observed. Finding it set means someone
            // called Thread.interrupt() (directly, or via
            // Future.cancel(true)) since the last chunk; reaching
            // llmux_cancel FROM HERE, inside the callback, is what the header
            // documents as safe — unlike llmux_close, which would deadlock
            // waiting on this very call.
            if (Thread.interrupted()) {
                state.interrupted = true;
                try {
                    state.lib.cancelGateway.invokeExact(state.handle);
                } catch (Throwable ignored) {
                    // Best-effort: we are already unwinding for interruption,
                    // and stream()'s own inspection of state.interrupted is
                    // what drives the outcome from here, not this call's
                    // success. A failure to even ask for cancellation still
                    // means "stop": returning 1 below still ends the stream.
                }
                return 1;
            }
            // The chunk is owned by the library and valid only for this call.
            // getString copies it before we hand it to Java code.
            String json = chunk.reinterpret(Long.MAX_VALUE).getString(0);
            state.chunks++;
            return state.handler.onChunk(json) ? 0 : 1;
        } catch (Throwable t) {
            state.thrown = t;
            return 1;
        }
    }

    // ---------------------------------------------------------------- closing

    /** Release the gateway. Idempotent, so error paths can close blindly. */
    @Override
    public void close() {
        long h;
        synchronized (this) {
            h = handle;
            handle = 0;
        }
        if (h == 0) {
            return;
        }
        try {
            lib.closeGateway.invokeExact(h);
        } catch (Throwable t) {
            throw new LlmuxException("llmux_close failed", t);
        }
    }

    // ---------------------------------------------------------------- helpers

    /** Read *errBox, free it, and return the message. Never returns null. */
    private static String takeError(Native lib, MemorySegment errBox) {
        MemorySegment p = errBox.get(ValueLayout.ADDRESS, 0);
        if (p.equals(MemorySegment.NULL)) {
            return "the library reported a failure but set no message";
        }
        String msg = p.reinterpret(Long.MAX_VALUE).getString(0);
        errBox.set(ValueLayout.ADDRESS, 0, MemorySegment.NULL);
        freeNative(lib, p);
        return msg;
    }

    /** Free *errBox if it is set, discarding the message. */
    private static void drainError(Native lib, MemorySegment errBox) {
        MemorySegment p = errBox.get(ValueLayout.ADDRESS, 0);
        if (!p.equals(MemorySegment.NULL)) {
            errBox.set(ValueLayout.ADDRESS, 0, MemorySegment.NULL);
            freeNative(lib, p);
        }
    }

    private static void freeNative(Native lib, MemorySegment p) {
        try {
            lib.free.invokeExact(p);
        } catch (Throwable t) {
            throw new LlmuxException("llmux_free failed", t);
        }
    }

    // ---------------------------------------------------------------- lookup

    /**
     * Locate libllmux, in order:
     * <ol>
     *   <li>{@code $LLMUX_LIBRARY} — an explicit path</li>
     *   <li>{@code $LLMUX_HOME/dist/ffi/<goos>_<goarch>/}</li>
     *   <li>{@code dist/ffi/<goos>_<goarch>/} walking up from the working
     *       directory — the layout {@code scripts/build-ffi.sh} produces</li>
     * </ol>
     *
     * @throws LlmuxException naming every path tried, and how to build one
     */
    public static Path findLibrary() {
        String explicit = System.getenv("LLMUX_LIBRARY");
        if (explicit != null && !explicit.isEmpty()) {
            Path p = Paths.get(explicit);
            if (!Files.isRegularFile(p)) {
                throw new LlmuxException("LLMUX_LIBRARY is set to " + p + ", which is not a file");
            }
            return p;
        }

        String file = libraryFileName();
        String dir = goos() + "_" + goarch();
        StringBuilder tried = new StringBuilder();

        String home = System.getenv("LLMUX_HOME");
        if (home != null && !home.isEmpty()) {
            Path p = Paths.get(home, "dist", "ffi", dir, file);
            if (Files.isRegularFile(p)) {
                return p;
            }
            tried.append("\n  ").append(p);
        }

        Path here = Paths.get("").toAbsolutePath();
        for (Path at = here; at != null; at = at.getParent()) {
            Path p = at.resolve("dist").resolve("ffi").resolve(dir).resolve(file);
            if (Files.isRegularFile(p)) {
                return p;
            }
            tried.append("\n  ").append(p);
        }

        throw new LlmuxException("no " + file + " found for " + dir + ". Tried:" + tried
                + "\nBuild one with `scripts/build-ffi.sh` in the llmux checkout, or set"
                + " LLMUX_LIBRARY to an existing library."
                + "\nPrebuilt libraries exist for darwin/arm64 and linux/arm64 only."
                + " There is no Windows DLL.");
    }

    private static String libraryFileName() {
        String os = goos();
        if (os.equals("darwin")) {
            return "libllmux.dylib";
        }
        if (os.equals("windows")) {
            return "llmux.dll";
        }
        return "libllmux.so";
    }

    private static String goos() {
        String os = System.getProperty("os.name", "").toLowerCase(Locale.ROOT);
        if (os.contains("mac") || os.contains("darwin")) {
            return "darwin";
        }
        if (os.contains("win")) {
            return "windows";
        }
        return "linux";
    }

    private static String goarch() {
        String arch = System.getProperty("os.arch", "").toLowerCase(Locale.ROOT);
        if (arch.equals("aarch64") || arch.equals("arm64")) {
            return "arm64";
        }
        if (arch.equals("x86_64") || arch.equals("amd64")) {
            return "amd64";
        }
        return arch;
    }
}
