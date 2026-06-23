using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Net.Http;
using System.Net.Sockets;
using System.Reflection;
using System.Text.RegularExpressions;
using System.Threading;
using Xunit;

namespace Llmux.Tests
{
    /// <summary>
    /// Tests for the llmux .NET sidecar launcher.
    ///
    /// Run from sdks/dotnet:  dotnet test tests/Llmux.Tests.csproj
    ///
    /// Covers binary resolution, URL formatting, health-poll readiness/timeout,
    /// singleton/lazy start, cleanup, and an integration test gated on the real
    /// binary. Most tests drive a fake fixture (a tiny python HTTP server
    /// honoring LLMUX_ADDR) via the LLMUX_BINARY override, so no real gateway is
    /// needed. The sidecar singleton is process-wide; xUnit runs tests in a
    /// class serially by default, which suits that.
    /// </summary>
    [Collection("sidecar")]
    public class SidecarTests : IDisposable
    {
        private static readonly string Fixture =
            Path.Combine(AppContext.BaseDirectory, "fixtures", "fake_llmux.py");

        public SidecarTests() => Sidecar.Stop();

        public void Dispose()
        {
            Sidecar.Stop();
            Environment.SetEnvironmentVariable("LLMUX_BINARY", null);
        }

        // --- helpers -------------------------------------------------------

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

        /// <summary>Write an executable shell wrapper that runs the python fake.</summary>
        private static string MakeFake(IDictionary<string, string>? extraEnv = null)
        {
            var py = Python() ?? throw new InvalidOperationException("python required");
            var dir = Path.Combine(Path.GetTempPath(), "llmux-dotnet-fake-" + Guid.NewGuid().ToString("N"));
            Directory.CreateDirectory(dir);
            var wrapper = Path.Combine(dir, "llmux");
            var exports = "";
            if (extraEnv != null)
            {
                foreach (var kv in extraEnv)
                {
                    exports += $"export {kv.Key}=\"{kv.Value}\"\n";
                }
            }
            File.WriteAllText(wrapper, $"#!/bin/sh\n{exports}exec \"{py}\" \"{Fixture}\"\n");
            Chmod755(wrapper);
            return wrapper;
        }

        private static void Chmod755(string path)
        {
            try
            {
                var psi = new ProcessStartInfo("chmod", $"755 \"{path}\"") { UseShellExecute = false };
                using var p = Process.Start(psi);
                p!.WaitForExit(2000);
            }
            catch
            {
                // best effort (Windows has no chmod; CI there uses the real binary)
            }
        }

        private static bool PortOpen(int port)
        {
            try
            {
                using var c = new TcpClient();
                var task = c.ConnectAsync("127.0.0.1", port);
                return task.Wait(300) && c.Connected;
            }
            catch
            {
                return false;
            }
        }

        private static bool WaitPortClosed(int port, TimeSpan timeout)
        {
            var deadline = DateTime.UtcNow + timeout;
            while (PortOpen(port) && DateTime.UtcNow < deadline)
            {
                Thread.Sleep(50);
            }
            return !PortOpen(port);
        }

        private static int HealthStatus(string baseUrl)
        {
            using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(1) };
            using var resp = http.GetAsync(baseUrl + "/health").GetAwaiter().GetResult();
            return (int)resp.StatusCode;
        }

        private static int PortOf(string baseUrl) =>
            int.Parse(baseUrl.Substring(baseUrl.LastIndexOf(':') + 1));

        private static bool PythonAvailable => Python() != null;

        // --- binary resolution --------------------------------------------

        [SkippableFact]
        public void EnvOverrideIsUsed()
        {
            Skip.IfNot(PythonAvailable, "python required for the fake fixture");
            Environment.SetEnvironmentVariable("LLMUX_BINARY", MakeFake());
            var baseUrl = Sidecar.Start();
            Assert.Equal(200, HealthStatus(baseUrl));
        }

        [SkippableFact]
        public void ClearErrorWhenBinaryMissing()
        {
            // Only assert the not-found path when nothing is resolvable.
            Environment.SetEnvironmentVariable("LLMUX_BINARY", null);
            var bundled = Path.Combine(AppContext.BaseDirectory, "bin", "llmux");
            Skip.If(File.Exists(bundled), "bundled bin present");

            // Invoke the private BinaryPath() via reflection with an empty PATH.
            var savedPath = Environment.GetEnvironmentVariable("PATH");
            Environment.SetEnvironmentVariable("PATH", "");
            try
            {
                var m = typeof(Sidecar).GetMethod("BinaryPath",
                    BindingFlags.NonPublic | BindingFlags.Static);
                var ex = Assert.Throws<TargetInvocationException>(() => m!.Invoke(null, null));
                Assert.IsType<LlmuxException>(ex.InnerException);
                Assert.Contains("llmux binary not found", ex.InnerException!.Message);
            }
            finally
            {
                Environment.SetEnvironmentVariable("PATH", savedPath);
            }
        }

        // --- URL formatting -----------------------------------------------

        [SkippableFact]
        public void OpenAiBaseUrlAppendsV1()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY", MakeFake());
            var baseUrl = Sidecar.BaseUrl();
            Assert.Matches(new Regex(@"^http://127\.0\.0\.1:\d+$"), baseUrl);
            Assert.Equal(baseUrl + "/v1", Sidecar.OpenAIBaseUrl());
            Assert.EndsWith("/v1", Sidecar.OpenAIBaseUrl());
        }

        // --- health-poll logic --------------------------------------------

        [SkippableFact]
        public void BecomesReadyOn200()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY", MakeFake());
            var baseUrl = Sidecar.Start(new Sidecar.Options { Timeout = TimeSpan.FromSeconds(10) });
            Assert.Equal(200, HealthStatus(baseUrl));
        }

        [SkippableFact]
        public void TimesOutWhenNever200()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY",
                MakeFake(new Dictionary<string, string> { ["FAKE_HEALTH_STATUS"] = "503" }));
            var ex = Assert.Throws<LlmuxException>(() =>
                Sidecar.Start(new Sidecar.Options { Timeout = TimeSpan.FromMilliseconds(600) }));
            Assert.Contains("did not become healthy", ex.Message);
        }

        [SkippableFact]
        public void TimesOutWhenUnreachable()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY",
                MakeFake(new Dictionary<string, string> { ["FAKE_NEVER_LISTEN"] = "1" }));
            Assert.Throws<LlmuxException>(() =>
                Sidecar.Start(new Sidecar.Options { Timeout = TimeSpan.FromMilliseconds(600) }));
        }

        // --- singleton / lazy start ---------------------------------------

        [SkippableFact]
        public void StartTwiceSameBaseNoRespawn()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY", MakeFake());
            var b1 = Sidecar.Start();
            var b2 = Sidecar.Start();
            var b3 = Sidecar.BaseUrl();
            Assert.Equal(b1, b2);
            Assert.Equal(b1, b3);
        }

        // --- cleanup -------------------------------------------------------

        [SkippableFact]
        public void StopKillsChildAndFreesPort()
        {
            Skip.IfNot(PythonAvailable, "python required");
            Environment.SetEnvironmentVariable("LLMUX_BINARY", MakeFake());
            var baseUrl = Sidecar.Start();
            var port = PortOf(baseUrl);
            Assert.True(PortOpen(port), "port should be open while running");
            Sidecar.Stop();
            Assert.True(WaitPortClosed(port, TimeSpan.FromSeconds(3)), "port should be freed");
        }

        // --- integration (real binary) ------------------------------------

        [SkippableFact]
        public void IntegrationRealBinary()
        {
            var real = Environment.GetEnvironmentVariable("LLMUX_BINARY_REAL");
            var bundled = Path.Combine(AppContext.BaseDirectory, "bin", "llmux");
            var bin = !string.IsNullOrEmpty(real) ? real : (File.Exists(bundled) ? bundled : null);
            Skip.If(bin == null, "real llmux binary not available");

            Environment.SetEnvironmentVariable("LLMUX_BINARY", bin);
            var baseUrl = Sidecar.Start(new Sidecar.Options { Timeout = TimeSpan.FromSeconds(15) });
            Assert.Matches(new Regex(@"^http://127\.0\.0\.1:\d+$"), baseUrl);
            Assert.Equal(200, HealthStatus(baseUrl));
            Assert.EndsWith("/v1", Sidecar.OpenAIBaseUrl());
        }
    }
}
