using System;
using System.Diagnostics;
using System.IO;
using System.Net.Http;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Xunit;

namespace Llmux.Tests
{
    /// <summary>
    /// Tests for LlmuxDirect's cancellation surface: <c>llmux_cancel</c> bound
    /// through <see cref="LlmuxDirect.Cancel"/>, and reached automatically when
    /// the <see cref="CancellationToken"/> passed to
    /// <see cref="LlmuxDirect.StreamAsync"/> is cancelled.
    ///
    /// Gated on two things this repo cannot assume: a python3 to run
    /// <c>sdks/fake-upstream.py</c> — the harness that COUNTS what the upstream
    /// actually generated, which is the whole point of these tests, since
    /// <c>ffi/fakeupstream</c> has no delay and no counter — and a libllmux
    /// built for this platform. Both gaps SKIP rather than FAIL: this project
    /// also runs on Windows CI, where there is no libllmux at all (see
    /// README.md), and a missing python3 says nothing about whether the
    /// binding itself is correct.
    ///
    /// Run from sdks/dotnet:  dotnet test tests/Llmux.Tests.csproj
    /// </summary>
    public class DirectCancelTests : IDisposable
    {
        // sdks/fake-upstream.py, a sibling of the dotnet/ SDK directory. Owned
        // by the coordinator (see BRIEF.md), so this test READS it and never
        // writes it.
        private static readonly string HarnessScript = Path.GetFullPath(
            Path.Combine(AppContext.BaseDirectory, "..", "..", "..", "..", "..", "fake-upstream.py"));

        private Process? _harness;

        public void Dispose()
        {
            if (_harness != null && !_harness.HasExited)
            {
                try
                {
                    _harness.Kill(entireProcessTree: true);
                }
                catch
                {
                    // best effort
                }
            }
            _harness?.Dispose();
        }

        // --- gating ----------------------------------------------------------

        private static string? Python()
        {
            foreach (var c in new[] { "python3", "python" })
            {
                try
                {
                    var psi = new ProcessStartInfo(c, "--version")
                    {
                        UseShellExecute = false,
                        RedirectStandardOutput = true,
                        RedirectStandardError = true,
                    };
                    using var p = Process.Start(psi);
                    p!.WaitForExit(3000);
                    return c;
                }
                catch
                {
                    // try next
                }
            }
            return null;
        }

        /// <summary>The library LlmuxDirect would find on its own, or null.</summary>
        private static string? Library()
        {
            try
            {
                return LlmuxDirect.FindLibrary();
            }
            catch (LlmuxException)
            {
                return null;
            }
        }

        // --- the harness -------------------------------------------------------

        private sealed record Harness(string ConfigJson, string BaseUrl);

        /// <summary>
        /// Starts sdks/fake-upstream.py and reads its two announcement lines.
        /// Mirrors sdks/dotnet/run-examples.sh's wait loop for the Go fake, but
        /// this one is read line-by-line off the child's own stdout pipe rather
        /// than polled through a temp file, since a test does not want a
        /// scratch directory of its own.
        /// </summary>
        private Harness StartHarness(string python, string text, int chunkDelayMs)
        {
            var psi = new ProcessStartInfo(python)
            {
                UseShellExecute = false,
                RedirectStandardOutput = true,
                RedirectStandardError = true,
            };
            psi.ArgumentList.Add(HarnessScript);
            psi.ArgumentList.Add("--text");
            psi.ArgumentList.Add(text);
            psi.ArgumentList.Add("--chunk-delay-ms");
            psi.ArgumentList.Add(chunkDelayMs.ToString());
            psi.ArgumentList.Add("--addr");
            psi.ArgumentList.Add("127.0.0.1:0");

            _harness = Process.Start(psi)
                ?? throw new InvalidOperationException("failed to start fake-upstream.py");

            string? url = null;
            string? config = null;
            var deadline = DateTime.UtcNow + TimeSpan.FromSeconds(5);
            while (DateTime.UtcNow < deadline && (url == null || config == null))
            {
                string? line = _harness.StandardOutput.ReadLine();
                if (line == null)
                {
                    break;
                }
                if (line.StartsWith("URL ", StringComparison.Ordinal))
                {
                    url = line.Substring("URL ".Length);
                }
                else if (line.StartsWith("CONFIG ", StringComparison.Ordinal))
                {
                    config = line.Substring("CONFIG ".Length);
                }
            }
            if (url == null || config == null)
            {
                throw new InvalidOperationException("fake-upstream.py never announced URL/CONFIG");
            }
            return new Harness(config, url);
        }

        private static JsonElement Generated(string baseUrl)
        {
            using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(2) };
            string body = http.GetStringAsync(baseUrl + "/generated").GetAwaiter().GetResult();
            return JsonDocument.Parse(body).RootElement.Clone();
        }

        private const string StreamRequest =
            """{"model":"demo","messages":[{"role":"user","content":"go"}],"stream":true}""";

        // --- tests -------------------------------------------------------------

        /// <summary>
        /// The measurement the whole feature exists to make small. Without
        /// llmux_cancel reaching the blocked native call, cancelling the token
        /// between chunks would do nothing until the NEXT chunk happened to
        /// arrive (llmux_stream sits in a network read the whole time, not in
        /// the callback) — so the upstream would keep generating and metering
        /// chunks nobody was going to see. This asserts it does not.
        /// </summary>
        [SkippableFact]
        public async Task CancellingTheTokenStopsTheUpstream()
        {
            string? python = Python();
            Skip.IfNot(python != null, "python3 required for sdks/fake-upstream.py");
            string? library = Library();
            Skip.IfNot(library != null, "no libllmux built for this platform");

            Harness harness = StartHarness(
                python!, "one two three four five six seven eight nine ten", chunkDelayMs: 50);

            using LlmuxDirect gw = LlmuxDirect.Open(harness.ConfigJson, library);

            using var cts = new CancellationTokenSource();
            int consumed = 0;
            await Assert.ThrowsAnyAsync<OperationCanceledException>(async () =>
            {
                await foreach (string _ in gw.StreamAsync(StreamRequest, "chat", cts.Token))
                {
                    consumed++;
                    if (consumed == 3)
                    {
                        // The idiomatic construct: cancel the token, not a
                        // call to Cancel() directly, so this exercises the
                        // registration StreamAsync sets up on our behalf.
                        cts.Cancel();
                    }
                }
            });

            // Cancellation must be prompt: no chunk already sitting in the
            // bounded channel gets yielded after the token fires.
            Assert.Equal(3, consumed);

            JsonElement generated = Generated(harness.BaseUrl);
            Assert.Equal(3, generated.GetProperty("generated").GetInt32());
        }

        /// <summary>
        /// A token cancelled before StreamAsync is even called must fail
        /// before any native work starts — no gateway call, no upstream
        /// request, nothing to clean up.
        /// </summary>
        [SkippableFact]
        public async Task AlreadyCancelledTokenThrowsBeforeAnyStreamStarts()
        {
            string? python = Python();
            Skip.IfNot(python != null, "python3 required for sdks/fake-upstream.py");
            string? library = Library();
            Skip.IfNot(library != null, "no libllmux built for this platform");

            Harness harness = StartHarness(python!, "one two three", chunkDelayMs: 50);
            using LlmuxDirect gw = LlmuxDirect.Open(harness.ConfigJson, library);

            using var cts = new CancellationTokenSource();
            cts.Cancel();

            await Assert.ThrowsAnyAsync<OperationCanceledException>(async () =>
            {
                await foreach (string _ in gw.StreamAsync(StreamRequest, "chat", cts.Token))
                {
                    // never reached
                }
            });

            JsonElement generated = Generated(harness.BaseUrl);
            Assert.Equal(0, generated.GetProperty("streams").GetInt32());
        }

        /// <summary>
        /// llmux_cancel on a closed handle is documented as a no-op; Cancel()
        /// must preserve that rather than throwing, so a cleanup path can call
        /// it unconditionally.
        /// </summary>
        [SkippableFact]
        public void CancelAfterDisposeIsANoOp()
        {
            string? library = Library();
            Skip.IfNot(library != null, "no libllmux built for this platform");

            LlmuxDirect gw = LlmuxDirect.Open(null, library);
            gw.Dispose();
            gw.Cancel();
        }
    }
}
