# SIDECAR STREAMING — token-by-token, over the sidecar's SSE endpoint.
#
#   cd sdks/elixir && mix run examples/sidecar_stream.exs
#
# Environment: LLMUX_BINARY, LLMUX_CONFIG, LLMUX_MODEL (default openai/gpt-4o-mini)
#
# This is the example that answers "but what about llmux_stream?". The C ABI
# delivers chunks by invoking a callback on the calling thread; a NIF wrapping
# it would run that callback on a BEAM scheduler thread, once per token, for the
# whole duration of a completion. Over SSE the same chunk objects arrive as
# ordinary Erlang messages, on a process the VM can preempt, kill, supervise and
# time out. README.md sets out the reasoning; this file is what it buys you:
# `stream/3` here yields each chunk to a function and can be cancelled at any
# point by just returning `:stop`.
#
# It speaks HTTP/1.1 over :gen_tcp rather than pulling in an HTTP client,
# because reading an SSE body as it arrives is the whole point and a
# request/response helper would hide it.

model = System.get_env("LLMUX_MODEL", "openai/gpt-4o-mini")

defmodule Sse do
  @moduledoc false

  @doc """
  POST `body` to `path` and invoke `fun` once per `data:` frame with the decoded
  chunk. `fun` returns `:stop` to end the stream early. Returns the chunk count.

  The socket is closed on every path, including a raise inside `fun` and an
  early stop — that is what the `try/after` is for.
  """
  def stream("http://" <> hostport = _base, path, body, fun) do
    [host, port] = String.split(hostport, ":", parts: 2)

    {:ok, sock} =
      :gen_tcp.connect(
        String.to_charlist(host),
        String.to_integer(port),
        [:binary, active: false, packet: :raw],
        5_000
      )

    try do
      request = [
        "POST ",
        path,
        " HTTP/1.1\r\n",
        "Host: ",
        hostport,
        "\r\n",
        "Authorization: Bearer llmux-local\r\n",
        "Content-Type: application/json\r\n",
        "Accept: text/event-stream\r\n",
        "Content-Length: ",
        Integer.to_string(byte_size(body)),
        "\r\n",
        "Connection: close\r\n\r\n",
        body
      ]

      :ok = :gen_tcp.send(sock, request)
      {status, rest} = read_head(sock, "")
      if status != 200, do: raise("sidecar answered HTTP #{status}")
      read_frames(sock, rest, fun, 0)
    after
      :gen_tcp.close(sock)
    end
  end

  defp read_head(sock, acc) do
    case :binary.split(acc, "\r\n\r\n") do
      [head, rest] ->
        ["HTTP/1." <> <<_::binary-size(1)>> <> " " <> <<code::binary-size(3)>> <> _ | _] =
          String.split(head, "\r\n")

        {String.to_integer(code), rest}

      _ ->
        {:ok, more} = :gen_tcp.recv(sock, 0, 30_000)
        read_head(sock, acc <> more)
    end
  end

  # Chunked transfer encoding plus SSE framing. Rather than decode chunk sizes,
  # scan for `data: ` lines: a size line never starts with one, and llmux writes
  # each event as a single `data: {...}\n\n`.
  defp read_frames(sock, buffer, fun, count) do
    case :binary.split(buffer, "\n") do
      [line, rest] ->
        case String.trim_trailing(line, "\r") do
          "data: [DONE]" ->
            count

          "data: " <> payload ->
            case fun.(:json.decode(payload)) do
              :stop -> count + 1
              _ -> read_frames(sock, rest, fun, count + 1)
            end

          _ ->
            read_frames(sock, rest, fun, count)
        end

      [partial] ->
        case :gen_tcp.recv(sock, 0, 30_000) do
          {:ok, more} -> read_frames(sock, partial <> more, fun, count)
          {:error, :closed} -> count
        end
    end
  end
end

{:ok, base} = Llmux.start()
IO.puts("sidecar : #{base}")

try do
  request =
    :json.encode(%{
      "model" => model,
      "messages" => [%{"role" => "user", "content" => "Say hello in five words."}],
      "stream" => true
    })
    |> IO.iodata_to_binary()

  IO.write("stream  : ")

  chunks =
    Sse.stream(base, "/v1/chat/completions", request, fn chunk ->
      # The last chunk carries usage and an empty "choices", so this cannot
      # assume there is a choice to read.
      delta =
        case chunk["choices"] do
          [%{"delta" => %{"content" => text}} | _] -> text
          _ -> nil
        end

      if is_binary(delta) and delta != "", do: IO.write(delta)
      :cont
    end)

  IO.puts("\n          (#{chunks} chunks)")

  # Stopping early. Over SSE this is a decision the consumer makes: return
  # :stop, the socket closes in the `after` above, and llmux sees the client go
  # away. The C ABI's equivalent is a callback returning non-zero.
  seen =
    Sse.stream(base, "/v1/chat/completions", request, fn _chunk ->
      :stop
    end)

  IO.puts("early   : stopped after #{seen} chunk, socket closed")

  # A streaming consumer that dies is a supervised process dying, not a VM
  # fault. This is the property a NIF would spend.
  parent = self()

  {pid, ref} =
    spawn_monitor(fn ->
      Sse.stream(base, "/v1/chat/completions", request, fn _ ->
        send(parent, :first_chunk)
        Process.sleep(:infinity)
      end)
    end)

  receive do: (:first_chunk -> :ok)
  Process.exit(pid, :kill)

  receive do
    {:DOWN, ^ref, :process, _, reason} ->
      IO.puts("isolate : killed a consumer mid-stream (#{inspect(reason)}); the VM is fine")
  end
after
  Llmux.stop()
end

IO.puts("stopped : ok")
