using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Http;
using System.Runtime.CompilerServices;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Llmux;

namespace Llmux.Examples
{
    /// <summary>
    /// llmux SIDECAR — the gateway as a child process on 127.0.0.1, over HTTP.
    /// The recommended default for .NET.
    ///
    /// No native library, no unsafe code, no platform matrix — it works on
    /// Windows, where the direct path has no library at all.
    ///
    /// Run it with sdks/dotnet/run-examples.sh.
    /// </summary>
    internal static class SidecarChat
    {
        internal static async Task<int> RunAsync()
        {
            // try/finally rather than `using`: the sidecar is a process-wide
            // singleton owned by the static class, and it must be stopped on the
            // error path too rather than left for the exit hook.
            string baseUrl = Sidecar.BaseUrl();
            using var http = new HttpClient { Timeout = TimeSpan.FromSeconds(30) };
            try
            {
                Console.WriteLine($"sidecar: {baseUrl}");
                Console.WriteLine($"openai base url: {Sidecar.OpenAIBaseUrl()}");

                // 1. The model list.
                string models = await http.GetStringAsync(baseUrl + "/v1/models");
                Console.WriteLine($"models: {DirectChat.OneLine(models, 160)}");

                // 2. One completion.
                const string request =
                    """{"model":"demo","messages":[{"role":"user","content":"hello"}]}""";
                string chat = await PostAsync(http, baseUrl, request);
                Console.WriteLine($"chat: {DirectChat.OneLine(chat, 200)}");

                // 3. Streaming, as the same IAsyncEnumerable shape the direct
                //    path offers — over SSE instead of a callback.
                const string streamRequest =
                    """{"model":"demo","messages":[{"role":"user","content":"hello"}],"stream":true}""";
                var text = new StringBuilder();
                int chunks = 0;
                await foreach (string chunk in StreamAsync(http, baseUrl, streamRequest))
                {
                    chunks++;
                    text.Append(DirectChat.Content(chunk));
                }
                Console.WriteLine($"stream: {chunks} chunks, text=\"{text}\"");

                // 4. Breaking out disposes the response stream, which drops the
                //    connection. How much the server had already generated is
                //    NOT observable from here — unlike the direct path, where
                //    the callback count is exact.
                int consumed = 0;
                await foreach (string chunk in StreamAsync(http, baseUrl, streamRequest))
                {
                    consumed++;
                    if (consumed == 2)
                    {
                        break;
                    }
                }
                Console.WriteLine($"early stop: consumed {consumed} chunk(s), response disposed");

                // 5. The error path is a status code, not an exception.
                string bad = await PostAsync(http, baseUrl, """{"model":"nope","messages":[]}""");
                Console.WriteLine($"error path: {DirectChat.OneLine(bad, 160)}");
            }
            finally
            {
                Sidecar.Stop();
                Console.WriteLine("sidecar stopped");
            }

            Console.WriteLine("done");
            return 0;
        }

        private static async Task<string> PostAsync(HttpClient http, string baseUrl, string body)
        {
            using var content = new StringContent(body, Encoding.UTF8, "application/json");
            using HttpResponseMessage response =
                await http.PostAsync(baseUrl + "/v1/chat/completions", content);
            return await response.Content.ReadAsStringAsync();
        }

        /// <summary>SSE `data:` frames as chunk JSON, with `[DONE]` stripped.</summary>
        private static async IAsyncEnumerable<string> StreamAsync(
            HttpClient http, string baseUrl, string requestJson,
            [EnumeratorCancellation] CancellationToken cancellationToken = default)
        {
            using var request = new HttpRequestMessage(
                HttpMethod.Post, baseUrl + "/v1/chat/completions")
            {
                Content = new StringContent(requestJson, Encoding.UTF8, "application/json"),
            };
            request.Headers.Accept.ParseAdd("text/event-stream");

            // ResponseHeadersRead: do not buffer the whole body before yielding
            // the first chunk, or "streaming" is a word rather than a behaviour.
            using HttpResponseMessage response = await http.SendAsync(
                request, HttpCompletionOption.ResponseHeadersRead, cancellationToken);
            using Stream body = await response.Content.ReadAsStreamAsync(cancellationToken);
            using var reader = new StreamReader(body, Encoding.UTF8);

            while (!reader.EndOfStream)
            {
                string? line = await reader.ReadLineAsync(cancellationToken);
                if (line == null)
                {
                    break;
                }
                if (!line.StartsWith("data: ", StringComparison.Ordinal))
                {
                    continue;
                }
                string payload = line.Substring("data: ".Length);
                if (payload == "[DONE]")
                {
                    break;
                }
                yield return payload;
            }
        }
    }
}
