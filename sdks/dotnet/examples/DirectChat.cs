using System;
using System.Collections.Generic;
using System.Net.Http;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using Llmux;

namespace Llmux.Examples
{
    /// <summary>
    /// llmux DIRECT — the gateway inside this process, through libllmux's C ABI.
    ///
    /// Run it with sdks/dotnet/run-examples.sh, which builds the shared library,
    /// starts the fake upstream from ffi/fakeupstream, and passes its
    /// configuration in LLMUX_CONFIG_JSON — so no API key and no network.
    ///
    /// READ sdks/dotnet/README.md FIRST. There is no Windows shared library, and
    /// for .NET that alone makes the sidecar the recommended default.
    /// </summary>
    internal static class DirectChat
    {
        internal static async Task<int> RunAsync()
        {
            string library = LlmuxDirect.FindLibrary();
            Console.WriteLine($"library: {library}");

            string? config = Environment.GetEnvironmentVariable("LLMUX_CONFIG_JSON");
            if (string.IsNullOrEmpty(config))
            {
                config = null;
            }

            // `using` disposes the SafeHandle on every path out of this scope,
            // including a thrown exception.
            using (var llmux = LlmuxDirect.Open(config, library))
            {
                Console.WriteLine($"abi version: {llmux.AbiVersion}");

                // 1. A call that needs no upstream.
                Console.WriteLine($"models: {OneLine(llmux.Call("models"), 160)}");

                if (config == null)
                {
                    Console.WriteLine();
                    Console.WriteLine("LLMUX_CONFIG_JSON is unset, so there is no upstream to talk to.");
                    Console.WriteLine("Skipping chat and streaming. Use run-examples.sh to get one.");
                    return 0;
                }

                // 2. One unary completion.
                const string request =
                    """{"model":"demo","messages":[{"role":"user","content":"hello"}]}""";
                Console.WriteLine($"chat: {OneLine(llmux.Call("chat", request), 200)}");

                // 3. The same completion as an IAsyncEnumerable.
                const string streamRequest =
                    """{"model":"demo","messages":[{"role":"user","content":"hello"}],"stream":true}""";
                var text = new System.Text.StringBuilder();
                int chunks = 0;
                await foreach (string chunk in llmux.StreamAsync(streamRequest))
                {
                    chunks++;
                    text.Append(Content(chunk));
                }
                Console.WriteLine($"stream: {chunks} chunks, text=\"{text}\"");

                // 4. Breaking out of the loop really stops the Go stream: the
                //    channel is bounded at one, so the native callback is
                //    blocked waiting for us and gets "stop" on its next turn.
                //    With an unbounded channel this would look identical and
                //    the whole completion would run — see README.md.
                int consumed = 0;
                await foreach (string chunk in llmux.StreamAsync(streamRequest))
                {
                    consumed++;
                    if (consumed == 2)
                    {
                        break;
                    }
                }
                Console.WriteLine($"early stop: consumed {consumed} chunk(s); the stream unwound early");

                // 5. Cancellation does the same thing, through the token — and
                //    does it by reaching llmux_cancel, not by waiting for the
                //    next chunk to happen to arrive. StreamAsync registers a
                //    callback on the token that calls Cancel() the instant it
                //    fires, which is what lets this throw promptly even while
                //    llmux_stream is blocked in a network read between
                //    chunks. See the measured version of this same demo
                //    below, and StreamAsync's remarks in LlmuxDirect.cs.
                using var cts = new CancellationTokenSource();
                int beforeCancel = 0;
                try
                {
                    await foreach (string chunk in llmux.StreamAsync(streamRequest, "chat", cts.Token))
                    {
                        beforeCancel++;
                        if (beforeCancel == 1)
                        {
                            cts.Cancel();
                        }
                    }
                }
                catch (OperationCanceledException)
                {
                    // Not llmux's own "context canceled" string — that is the
                    // whole point of routing this through the token rather
                    // than surfacing the library's error text.
                    Console.WriteLine($"cancellation: token cancelled after {beforeCancel} chunk(s)");
                }

                // 6. The measured version of the same demo, against the
                //    harness built for this: sdks/fake-upstream.py sleeps
                //    between chunks and counts what actually reached the
                //    wire at GET /generated. ffi/fakeupstream (used for
                //    everything above) has neither, so it cannot answer the
                //    question that matters: did cancelling really stop the
                //    provider from generating and metering more tokens, or
                //    did it just stop telling this process about them?
                //    run-examples.sh starts this second fake and passes its
                //    config and URL here; without it, this step is skipped
                //    the same way step 1 skips chat and streaming when there
                //    is no upstream at all.
                string? cancelConfig = Environment.GetEnvironmentVariable("LLMUX_CANCEL_CONFIG_JSON");
                string? cancelUrl = Environment.GetEnvironmentVariable("LLMUX_CANCEL_URL");
                if (!string.IsNullOrEmpty(cancelConfig) && !string.IsNullOrEmpty(cancelUrl))
                {
                    using var cancelGateway = LlmuxDirect.Open(cancelConfig, library);
                    const string tenWordsRequest =
                        """{"model":"demo","messages":[{"role":"user","content":"go"}],"stream":true}""";

                    using var measureCts = new CancellationTokenSource();
                    int seen = 0;
                    try
                    {
                        await foreach (string chunk in
                            cancelGateway.StreamAsync(tenWordsRequest, "chat", measureCts.Token))
                        {
                            seen++;
                            if (seen == 3)
                            {
                                // The idiomatic construct — cancel the token,
                                // not Cancel() directly — so this exercises
                                // the same registration a real consumer would
                                // rely on.
                                measureCts.Cancel();
                            }
                        }
                    }
                    catch (OperationCanceledException)
                    {
                        // Expected; see step 5.
                    }

                    using var http = new HttpClient();
                    string generatedJson = await http.GetStringAsync(cancelUrl.TrimEnd('/') + "/generated");
                    Console.WriteLine($"cancellation (measured): consumer saw {seen} chunk(s); "
                        + $"upstream reports {generatedJson}");
                }
                else
                {
                    Console.WriteLine();
                    Console.WriteLine("LLMUX_CANCEL_CONFIG_JSON/LLMUX_CANCEL_URL unset: skipping the measured");
                    Console.WriteLine("cancellation demo. Use run-examples.sh for the harness with a 100 ms");
                    Console.WriteLine("per-chunk delay and a GET /generated counter.");
                }

                // 7. The error path. The library's message becomes an exception
                //    and the C string it allocated for it is freed.
                try
                {
                    llmux.Call("no-such-method", "{}");
                    Console.WriteLine("FAIL: a bogus method should not have succeeded");
                }
                catch (LlmuxException e)
                {
                    Console.WriteLine($"error path: {e.Message}");
                }
            }

            // 8. Use after dispose is a clean error, not a crash.
            var closed = LlmuxDirect.Open(config, library);
            closed.Dispose();
            closed.Dispose(); // idempotent
            try
            {
                closed.Call("models");
                Console.WriteLine("FAIL: a disposed gateway should not have answered");
            }
            catch (LlmuxException e)
            {
                Console.WriteLine($"after dispose: {e.Message}");
            }

            Console.WriteLine("done");
            return 0;
        }

        /// <summary>Pull assistant text out of a chunk without a JSON dependency.</summary>
        internal static string Content(string chunkJson)
        {
            const string needle = "\"content\":\"";
            int i = chunkJson.IndexOf(needle, StringComparison.Ordinal);
            if (i < 0)
            {
                return string.Empty;
            }
            i += needle.Length;
            var sb = new System.Text.StringBuilder();
            while (i < chunkJson.Length && chunkJson[i] != '"')
            {
                char c = chunkJson[i];
                if (c == '\\' && i + 1 < chunkJson.Length)
                {
                    i++;
                    c = chunkJson[i] == 'n' ? '\n' : chunkJson[i];
                }
                sb.Append(c);
                i++;
            }
            return sb.ToString();
        }

        internal static string OneLine(string s, int max)
        {
            string flat = Regex.Replace(s, @"\s+", " ");
            return flat.Length <= max ? flat : flat.Substring(0, max) + "…";
        }
    }
}
