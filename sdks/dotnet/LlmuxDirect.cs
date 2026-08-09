using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Reflection;
using System.Runtime.CompilerServices;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Channels;
using System.Threading.Tasks;

namespace Llmux
{
    /// <summary>
    /// llmux <b>in this process</b>, through the C ABI of <c>libllmux</c>.
    ///
    /// <para><see cref="Sidecar"/> is the other path: it spawns <c>llmux serve</c>
    /// and talks HTTP. <b>On .NET the sidecar is the recommended default</b> —
    /// not because this is broken, but because there is no Windows shared
    /// library and .NET's Windows install base is large. See README.md.</para>
    ///
    /// <para>JSON in, JSON out: the same JSON <c>POST /v1/chat/completions</c>
    /// takes and returns. This class deliberately does not parse it, and has no
    /// JSON dependency.</para>
    ///
    /// <code>
    /// using var llmux = LlmuxDirect.Open(configJson);
    /// string models = llmux.Call("models");
    /// await foreach (string chunk in llmux.StreamAsync(requestJson, ct))
    /// {
    ///     Console.Write(chunk);
    /// }
    /// </code>
    /// </summary>
    public sealed unsafe partial class LlmuxDirect : IDisposable
    {
        // ------------------------------------------------------------ P/Invoke
        //
        // LibraryImport (the source-generated marshaller, .NET 7+) rather than
        // DllImport: the marshalling stubs are generated at compile time,
        // AOT-compatible, and — the reason that matters here — every string is
        // declared explicitly. A `string` return type would have the runtime
        // copy the C string and then LEAK it, because it has no idea the memory
        // must go back to llmux_free. Returns are therefore IntPtr, always.

        private const string Lib = "llmux";

        [LibraryImport(Lib, EntryPoint = "llmux_abi_version")]
        private static partial IntPtr AbiVersionNative();

        [LibraryImport(Lib, EntryPoint = "llmux_new", StringMarshalling = StringMarshalling.Utf8)]
        private static partial ulong NewNative(string? configJson, IntPtr* err);

        [LibraryImport(Lib, EntryPoint = "llmux_close")]
        private static partial void CloseNative(ulong handle);

        [LibraryImport(Lib, EntryPoint = "llmux_call", StringMarshalling = StringMarshalling.Utf8)]
        private static partial IntPtr CallNative(ulong handle, string method, string? requestJson, IntPtr* err);

        [LibraryImport(Lib, EntryPoint = "llmux_free")]
        private static partial void FreeNative(IntPtr p);

        [LibraryImport(Lib, EntryPoint = "llmux_stream", StringMarshalling = StringMarshalling.Utf8)]
        private static partial int StreamNative(
            ulong handle, string method, string? requestJson,
            delegate* unmanaged<IntPtr, IntPtr, int> callback, IntPtr userData, IntPtr* err);

        // ------------------------------------------------------ library lookup

        private static int _resolverInstalled;

        /// <summary>
        /// Point the runtime at a specific libllmux. Call before the first use,
        /// or leave it and let <see cref="FindLibrary"/> decide.
        /// </summary>
        public static void UseLibrary(string path)
        {
            if (!File.Exists(path))
            {
                throw new LlmuxException($"no libllmux at {path}");
            }
            _explicitPath = path;
            InstallResolver();
        }

        private static string? _explicitPath;

        private static void InstallResolver()
        {
            if (Interlocked.Exchange(ref _resolverInstalled, 1) == 1)
            {
                return;
            }
            NativeLibrary.SetDllImportResolver(
                Assembly.GetExecutingAssembly(),
                (name, assembly, searchPath) =>
                    name == Lib ? NativeLibrary.Load(_explicitPath ?? FindLibrary()) : IntPtr.Zero);
        }

        /// <summary>
        /// Locate libllmux: <c>$LLMUX_LIBRARY</c>, then
        /// <c>$LLMUX_HOME/dist/ffi/&lt;goos&gt;_&lt;goarch&gt;/</c>, then
        /// <c>dist/ffi/&lt;goos&gt;_&lt;goarch&gt;/</c> walking up from the
        /// current directory — the layout <c>scripts/build-ffi.sh</c> writes.
        /// </summary>
        public static string FindLibrary()
        {
            string? explicitPath = Environment.GetEnvironmentVariable("LLMUX_LIBRARY");
            if (!string.IsNullOrEmpty(explicitPath))
            {
                if (!File.Exists(explicitPath))
                {
                    throw new LlmuxException($"LLMUX_LIBRARY is set to {explicitPath}, which is not a file");
                }
                return explicitPath!;
            }

            string file = RuntimeInformation.IsOSPlatform(OSPlatform.OSX) ? "libllmux.dylib"
                        : RuntimeInformation.IsOSPlatform(OSPlatform.Windows) ? "llmux.dll"
                        : "libllmux.so";
            string dir = GoOs() + "_" + GoArch();
            var tried = new List<string>();

            string? home = Environment.GetEnvironmentVariable("LLMUX_HOME");
            if (!string.IsNullOrEmpty(home))
            {
                string p = Path.Combine(home!, "dist", "ffi", dir, file);
                if (File.Exists(p)) { return p; }
                tried.Add(p);
            }

            for (string? at = Directory.GetCurrentDirectory(); at != null; at = Path.GetDirectoryName(at))
            {
                string p = Path.Combine(at, "dist", "ffi", dir, file);
                if (File.Exists(p)) { return p; }
                tried.Add(p);
            }

            throw new LlmuxException(
                $"no {file} found for {dir}. Tried:{Environment.NewLine}  "
                + string.Join(Environment.NewLine + "  ", tried)
                + Environment.NewLine
                + "Build one with `scripts/build-ffi.sh` in the llmux checkout, or set LLMUX_LIBRARY."
                + Environment.NewLine
                + "Prebuilt libraries exist for darwin/arm64 and linux/arm64 only. THERE IS NO WINDOWS DLL.");
        }

        private static string GoOs() =>
            RuntimeInformation.IsOSPlatform(OSPlatform.OSX) ? "darwin"
            : RuntimeInformation.IsOSPlatform(OSPlatform.Windows) ? "windows"
            : "linux";

        private static string GoArch() => RuntimeInformation.ProcessArchitecture switch
        {
            Architecture.Arm64 => "arm64",
            Architecture.X64 => "amd64",
            var other => other.ToString().ToLowerInvariant(),
        };

        // ------------------------------------------------------------- handle

        /// <summary>
        /// The gateway handle, as a <see cref="SafeHandle"/> so it is released
        /// deterministically by <c>using</c> and, failing that, by the
        /// finalizer the base class provides — rather than never.
        ///
        /// <para>llmux handles are uint64 registry keys, not pointers. SafeHandle
        /// is still the right vehicle: it is the type the runtime knows how to
        /// keep alive across a P/Invoke and release exactly once. The key is
        /// stored in the IntPtr, which is 64 bits on every platform llmux ships
        /// a library for.</para>
        /// </summary>
        public sealed class LlmuxSafeHandle : SafeHandle
        {
            public LlmuxSafeHandle() : base(IntPtr.Zero, ownsHandle: true) { }

            internal LlmuxSafeHandle(ulong h) : base(IntPtr.Zero, ownsHandle: true)
            {
                SetHandle((IntPtr)h);
            }

            /// <summary>0 is never a valid llmux handle.</summary>
            public override bool IsInvalid => handle == IntPtr.Zero;

            internal ulong Value => (ulong)handle;

            protected override bool ReleaseHandle()
            {
                // llmux_close is idempotent and tolerates unknown handles, so
                // this cannot fail; it also must not throw — a throwing
                // ReleaseHandle is undefined behaviour.
                CloseNative((ulong)handle);
                return true;
            }
        }

        private readonly LlmuxSafeHandle _handle;

        private LlmuxDirect(LlmuxSafeHandle handle, string version, string libraryPath)
        {
            _handle = handle;
            AbiVersion = version;
            LibraryPath = libraryPath;
        }

        /// <summary>The llmux version the loaded library was built from.</summary>
        public string AbiVersion { get; }

        /// <summary>The library this gateway is running out of.</summary>
        public string LibraryPath { get; }

        // ------------------------------------------------------------- opening

        /// <summary>
        /// Open a gateway.
        /// </summary>
        /// <param name="configJson">
        /// An llmux configuration document — the same JSON <c>llmux serve</c>
        /// reads. <c>null</c> means built-in defaults plus the environment.
        /// </param>
        /// <param name="libraryPath">An explicit libllmux, or null to search.</param>
        public static LlmuxDirect Open(string? configJson = null, string? libraryPath = null)
        {
            if (libraryPath != null)
            {
                UseLibrary(libraryPath);
            }
            else
            {
                InstallResolver();
            }
            string resolved = _explicitPath ?? FindLibrary();

            string version = ReadStaticString(AbiVersionNative(), "llmux_abi_version");

            IntPtr err = IntPtr.Zero;
            ulong h = NewNative(configJson, &err);
            if (h == 0)
            {
                throw new LlmuxException("llmux_new: " + TakeError(ref err));
            }
            DrainError(ref err);
            return new LlmuxDirect(new LlmuxSafeHandle(h), version, resolved);
        }

        // ------------------------------------------------------------- calling

        /// <summary>
        /// One unary call. <paramref name="method"/> is <c>"chat"</c>,
        /// <c>"embed"</c> or <c>"models"</c>.
        /// </summary>
        /// <exception cref="LlmuxException">carrying the library's own message.</exception>
        public string Call(string method, string? requestJson = null)
        {
            ArgumentNullException.ThrowIfNull(method);
            bool taken = false;
            try
            {
                // DangerousAddRef pins the handle open for the duration of the
                // call, so a concurrent Dispose cannot close it mid-flight.
                _handle.DangerousAddRef(ref taken);
                if (_handle.IsInvalid)
                {
                    throw new LlmuxException("this gateway is closed");
                }

                IntPtr err = IntPtr.Zero;
                IntPtr result = CallNative(_handle.Value, method, requestJson, &err);
                if (result == IntPtr.Zero)
                {
                    throw new LlmuxException($"llmux_call({method}): " + TakeError(ref err));
                }
                try
                {
                    // Copy into a managed string, then hand the C allocation
                    // back. Marshal.FreeHGlobal would be wrong here: it was not
                    // allocated by our allocator.
                    return Marshal.PtrToStringUTF8(result)
                        ?? throw new LlmuxException("llmux_call returned a string that is not UTF-8");
                }
                finally
                {
                    FreeNative(result);
                    DrainError(ref err);
                }
            }
            catch (ObjectDisposedException)
            {
                throw new LlmuxException("this gateway is closed");
            }
            finally
            {
                if (taken)
                {
                    _handle.DangerousRelease();
                }
            }
        }

        // ----------------------------------------------------------- streaming

        /// <summary>
        /// Stream a chat completion as an <see cref="IAsyncEnumerable{T}"/> of
        /// <c>chat.completion.chunk</c> JSON documents.
        ///
        /// <para><b>Stopping early really stops the stream.</b> The enumerator
        /// hands each chunk over a bounded channel of capacity one, so the
        /// native callback blocks until you have taken the previous chunk.
        /// Break out of the <c>await foreach</c>, or cancel the token, and the
        /// next callback returns "stop" and <c>llmux_stream</c> unwinds. An
        /// unbounded channel would look identical and quietly let the whole
        /// completion run — measured, see README.md.</para>
        ///
        /// <para>Expect one chunk beyond your last consumed one: cancellation is
        /// prompt, not retroactive.</para>
        /// </summary>
        public async IAsyncEnumerable<string> StreamAsync(
            string requestJson,
            string method = "chat",
            [EnumeratorCancellation] CancellationToken cancellationToken = default)
        {
            ArgumentNullException.ThrowIfNull(requestJson);

            // Capacity 1 is load-bearing. See the remarks above.
            var channel = Channel.CreateBounded<string>(new BoundedChannelOptions(1)
            {
                SingleReader = true,
                SingleWriter = true,
                FullMode = BoundedChannelFullMode.Wait,
            });

            var state = new StreamState(channel.Writer, cancellationToken);
            var gcHandle = GCHandle.Alloc(state);

            // llmux_stream blocks its thread for the whole stream, so it gets a
            // dedicated one rather than a thread-pool worker held hostage.
            var pump = Task.Factory.StartNew(
                () => RunStream(method, requestJson, state, gcHandle, channel.Writer),
                CancellationToken.None, TaskCreationOptions.LongRunning, TaskScheduler.Default);

            try
            {
                await foreach (string chunk in channel.Reader.ReadAllAsync(cancellationToken)
                                   .ConfigureAwait(false))
                {
                    yield return chunk;
                }
            }
            finally
            {
                // However we leave — completion, break, or cancellation — tell
                // the producer to stop and wait for the native call to unwind
                // before releasing the GCHandle it is still dereferencing.
                state.Stop();
                channel.Writer.TryComplete();
                try
                {
                    await pump.ConfigureAwait(false);
                }
                catch (LlmuxException) when (state.Stopped)
                {
                    // We asked it to stop; its complaint is not news.
                }
                finally
                {
                    gcHandle.Free();
                }
            }
        }

        /// <summary>
        /// The blocking half of <see cref="StreamAsync"/>, on its own thread.
        ///
        /// <para>This is a separate method because C# forbids pointer types in an
        /// async or iterator method body, and <c>StreamAsync</c> is both. The
        /// split is a language constraint, not a design choice.</para>
        /// </summary>
        private void RunStream(
            string method, string requestJson, StreamState state,
            GCHandle gcHandle, ChannelWriter<string> writer)
        {
            bool taken = false;
            try
            {
                _handle.DangerousAddRef(ref taken);
                if (_handle.IsInvalid)
                {
                    throw new LlmuxException("this gateway is closed");
                }
                IntPtr err = IntPtr.Zero;
                int rc = StreamNative(
                    _handle.Value, method, requestJson,
                    &OnChunk, GCHandle.ToIntPtr(gcHandle), &err);
                if (state.Thrown != null)
                {
                    DrainError(ref err);
                    throw new LlmuxException("the chunk handler threw", state.Thrown);
                }
                if (rc != 0)
                {
                    throw new LlmuxException($"llmux_stream({method}): " + TakeError(ref err));
                }
                DrainError(ref err);
                writer.TryComplete();
            }
            catch (Exception e)
            {
                writer.TryComplete(e);
            }
            finally
            {
                if (taken)
                {
                    _handle.DangerousRelease();
                }
            }
        }

        private sealed class StreamState
        {
            private readonly ChannelWriter<string> _writer;
            private readonly CancellationToken _token;
            private volatile bool _stopped;

            internal StreamState(ChannelWriter<string> writer, CancellationToken token)
            {
                _writer = writer;
                _token = token;
            }

            internal Exception? Thrown { get; private set; }

            internal bool Stopped => _stopped;

            internal void Stop() => _stopped = true;

            /// <returns>0 to keep streaming, non-zero to stop.</returns>
            internal int Offer(IntPtr chunk)
            {
                try
                {
                    if (_stopped || _token.IsCancellationRequested)
                    {
                        return 1;
                    }
                    string? json = Marshal.PtrToStringUTF8(chunk);
                    if (json == null)
                    {
                        return 1;
                    }
                    // Blocks until the consumer has taken the previous chunk;
                    // that block is the backpressure the docs promise.
                    _writer.WriteAsync(json, _token).AsTask().GetAwaiter().GetResult();
                    return 0;
                }
                catch (OperationCanceledException)
                {
                    _stopped = true;
                    return 1;
                }
                catch (ChannelClosedException)
                {
                    _stopped = true;
                    return 1;
                }
                catch (Exception e)
                {
                    Thrown = e;
                    return 1;
                }
            }
        }

        /// <summary>
        /// The native chunk callback.
        ///
        /// <para><b>Nothing may escape this method.</b> An exception crossing an
        /// <c>UnmanagedCallersOnly</c> boundary terminates the process, exactly
        /// as it does through a JNI or FFM upcall, so every path here is
        /// wrapped and a failure is turned into "stop the stream", which
        /// llmux_stream honours cleanly.</para>
        /// </summary>
        [UnmanagedCallersOnly]
        private static int OnChunk(IntPtr chunkJson, IntPtr userData)
        {
            try
            {
                if (userData == IntPtr.Zero)
                {
                    return 1;
                }
                var state = (StreamState?)GCHandle.FromIntPtr(userData).Target;
                return state?.Offer(chunkJson) ?? 1;
            }
            catch
            {
                return 1;
            }
        }

        // ------------------------------------------------------------ disposal

        /// <summary>
        /// Release the gateway. Idempotent — <c>using</c> and an explicit
        /// Dispose on an error path cannot double-close.
        /// </summary>
        public void Dispose() => _handle.Dispose();

        // ------------------------------------------------------------- helpers

        private static string ReadStaticString(IntPtr p, string what)
        {
            if (p == IntPtr.Zero)
            {
                throw new LlmuxException($"{what} returned NULL");
            }
            // Static, owned by the library: read it, never free it.
            return Marshal.PtrToStringUTF8(p) ?? throw new LlmuxException($"{what} is not UTF-8");
        }

        /// <summary>Read the error, free it, and null the out-parameter.</summary>
        private static string TakeError(ref IntPtr err)
        {
            if (err == IntPtr.Zero)
            {
                return "the library reported a failure but set no message";
            }
            string message = Marshal.PtrToStringUTF8(err) ?? "(error message is not UTF-8)";
            FreeNative(err);
            err = IntPtr.Zero;
            return message;
        }

        /// <summary>
        /// Free the error out-parameter even on the SUCCESS path, so a library
        /// that sets a message alongside a success cannot leak it.
        /// </summary>
        private static void DrainError(ref IntPtr err)
        {
            if (err != IntPtr.Zero)
            {
                FreeNative(err);
                err = IntPtr.Zero;
            }
        }
    }
}
