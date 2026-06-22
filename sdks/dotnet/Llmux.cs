using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Runtime.InteropServices;
using System.Threading;

namespace Llmux
{
    /// <summary>
    /// llmux — the LLM multiplexer, embedded locally for .NET.
    ///
    /// The local wedge: instead of running a server yourself, this starts the
    /// gateway as a child process on 127.0.0.1 and hands you a base URL. Point
    /// any OpenAI-compatible client at it.
    ///
    /// <code>
    ///   string baseUrl = Llmux.Sidecar.BaseUrl();        // http://127.0.0.1:&lt;port&gt;
    ///   string v1 = Llmux.Sidecar.OpenAIBaseUrl();        // …/v1
    /// </code>
    ///
    /// Provider keys are inherited from the environment (OPENAI_API_KEY,
    /// ANTHROPIC_API_KEY, GEMINI_API_KEY, …).
    /// </summary>
    public static class Sidecar
    {
        public const string Version = "0.1.0";

        private static readonly object Gate = new object();
        private static Process? _proc;
        private static string? _base;
        private static bool _exitHooked;
        private static readonly HttpClient Http = new HttpClient
        {
            Timeout = TimeSpan.FromSeconds(1),
        };

        public sealed class Options
        {
            /// <summary>Fixed port; defaults to an ephemeral free port.</summary>
            public int? Port { get; set; }
            /// <summary>Path to a JSON config file.</summary>
            public string? Config { get; set; }
            /// <summary>Extra environment variables for the child process.</summary>
            public IDictionary<string, string>? Env { get; set; }
            /// <summary>Health-check timeout (default 10s).</summary>
            public TimeSpan? Timeout { get; set; }
        }

        /// <summary>Start the sidecar (idempotent). Returns the base URL.</summary>
        public static string Start(Options? opts = null)
        {
            lock (Gate)
            {
                if (_proc != null && !_proc.HasExited)
                {
                    return _base!;
                }
                opts ??= new Options();

                int port = opts.Port ?? FreePort();
                string addr = $"127.0.0.1:{port}";

                var psi = new ProcessStartInfo
                {
                    FileName = BinaryPath(),
                    UseShellExecute = false,
                };
                psi.Environment["LLMUX_ADDR"] = addr;
                if (opts.Config != null)
                {
                    psi.Environment["LLMUX_CONFIG"] = opts.Config;
                }
                if (opts.Env != null)
                {
                    foreach (var kv in opts.Env)
                    {
                        psi.Environment[kv.Key] = kv.Value;
                    }
                }

                Process proc;
                try
                {
                    proc = Process.Start(psi)
                        ?? throw new LlmuxException("failed to spawn llmux binary");
                }
                catch (Exception e) when (e is not LlmuxException)
                {
                    throw new LlmuxException("failed to spawn llmux binary", e);
                }
                _proc = proc;
                _base = $"http://{addr}";

                try
                {
                    WaitHealthy(_base, opts.Timeout ?? TimeSpan.FromSeconds(10));
                }
                catch
                {
                    Stop();
                    throw;
                }

                if (!_exitHooked)
                {
                    AppDomain.CurrentDomain.ProcessExit += (_, _) => Stop();
                    Console.CancelKeyPress += (_, _) => Stop();
                    _exitHooked = true;
                }
                return _base;
            }
        }

        /// <summary>The running base URL, starting the sidecar if needed.</summary>
        public static string BaseUrl()
        {
            lock (Gate)
            {
                if (_proc != null && !_proc.HasExited)
                {
                    return _base!;
                }
            }
            return Start();
        }

        /// <summary>The OpenAI-style base URL (…/v1).</summary>
        public static string OpenAIBaseUrl() => BaseUrl() + "/v1";

        /// <summary>Stop the sidecar if running.</summary>
        public static void Stop()
        {
            lock (Gate)
            {
                if (_proc != null && !_proc.HasExited)
                {
                    try
                    {
                        _proc.Kill(entireProcessTree: true);
                        _proc.WaitForExit(5000);
                    }
                    catch
                    {
                        // best effort
                    }
                }
                _proc = null;
            }
        }

        private static string BinaryPath()
        {
            // 1) explicit override
            string? env = Environment.GetEnvironmentVariable("LLMUX_BINARY");
            if (!string.IsNullOrEmpty(env))
            {
                return env!;
            }
            // 2) binary bundled next to the assembly
            bool windows = RuntimeInformation.IsOSPlatform(OSPlatform.Windows);
            string name = windows ? "llmux.exe" : "llmux";
            string baseDir = AppContext.BaseDirectory;
            string bundled = Path.Combine(baseDir, "bin", name);
            if (File.Exists(bundled))
            {
                return bundled;
            }
            // 3) on PATH
            string? found = Which("llmux", windows);
            if (found != null)
            {
                return found;
            }
            throw new LlmuxException(
                "llmux binary not found. Set LLMUX_BINARY, or build it: " +
                "`go build -o sdks/dotnet/bin/llmux ./cmd/llmux`");
        }

        private static string? Which(string cmd, bool windows)
        {
            string? path = Environment.GetEnvironmentVariable("PATH");
            if (path == null)
            {
                return null;
            }
            foreach (string dir in path.Split(Path.PathSeparator))
            {
                string candidate = Path.Combine(dir, cmd);
                if (File.Exists(candidate))
                {
                    return candidate;
                }
                if (windows)
                {
                    string exe = Path.Combine(dir, cmd + ".exe");
                    if (File.Exists(exe))
                    {
                        return exe;
                    }
                }
            }
            return null;
        }

        private static int FreePort()
        {
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();
            int port = ((IPEndPoint)listener.LocalEndpoint).Port;
            listener.Stop();
            return port;
        }

        private static void WaitHealthy(string baseUrl, TimeSpan timeout)
        {
            var deadline = DateTime.UtcNow + timeout;
            string last = "connection refused";
            while (DateTime.UtcNow < deadline)
            {
                try
                {
                    using var resp = Http.GetAsync(baseUrl + "/health").GetAwaiter().GetResult();
                    if (resp.StatusCode == HttpStatusCode.OK)
                    {
                        return;
                    }
                    last = $"status {(int)resp.StatusCode}";
                }
                catch (Exception e)
                {
                    last = e.Message;
                }
                Thread.Sleep(50);
            }
            throw new LlmuxException($"llmux did not become healthy within {timeout}: {last}");
        }
    }

    /// <summary>Thrown when the sidecar cannot be located, started, or made healthy.</summary>
    public sealed class LlmuxException : Exception
    {
        public LlmuxException(string message) : base(message) { }
        public LlmuxException(string message, Exception inner) : base(message, inner) { }
    }
}
