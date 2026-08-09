# SIDECAR (out-of-process) — the SDK spawns `llmux serve` on a loopback port
# through an Erlang Port, waits for it to be healthy, and reaps it when the
# GenServer stops. You never run a server by hand.
#
#   cd sdks/elixir && mix run examples/sidecar_chat.exs
#
# Environment:
#   LLMUX_BINARY  path to the llmux binary (default: bundled priv/bin/llmux, then PATH)
#   LLMUX_CONFIG  path to an llmux.json (optional)
#   LLMUX_MODEL   model to ask (default: openai/gpt-4o-mini)
#
# For Elixir this is not the "fallback" mode — it is the mode. README.md sets
# out why an in-process NIF is the wrong trade for this library.

# :httpc lives in :inets and reaches for :ssl/:public_key defaults even for a
# plain-http request, so all three have to be available.
Mix.ensure_application!(:public_key)
Mix.ensure_application!(:ssl)
Mix.ensure_application!(:inets)
:inets.start()

model = System.get_env("LLMUX_MODEL", "openai/gpt-4o-mini")

defmodule Demo do
  @headers [{~c"authorization", ~c"Bearer llmux-local"}]

  def get(url) do
    case :httpc.request(:get, {String.to_charlist(url), @headers}, [{:timeout, 30_000}], []) do
      {:ok, {{_, status, _}, _headers, body}} -> {:ok, status, IO.iodata_to_binary(body)}
      {:error, reason} -> {:error, reason}
    end
  end

  def post(url, body) do
    request =
      {String.to_charlist(url), @headers, ~c"application/json", :erlang.binary_to_list(body)}

    case :httpc.request(:post, request, [{:timeout, 120_000}], []) do
      {:ok, {{_, status, _}, _headers, resp}} -> {:ok, status, IO.iodata_to_binary(resp)}
      {:error, reason} -> {:error, reason}
    end
  end

  # A three-line JSON reader, so the example has no dependency to install. Any
  # real program uses Jason or :json (OTP 27+).
  def decode(body), do: :json.decode(body)

  def encode(term), do: term |> :json.encode() |> IO.iodata_to_binary()
end

# Starts the child process and waits for GET /health. Idempotent.
{:ok, base} =
  case Llmux.start() do
    {:ok, base} ->
      {:ok, base}

    {:error, reason} ->
      IO.puts(:stderr, "could not start the sidecar: #{inspect(reason)}")
      System.halt(1)
  end

IO.puts("sidecar : #{base}")
{:ok, v1} = Llmux.openai_base_url()

# try/after, so a failure below still stops the child rather than leaving an
# orphaned server holding a port. Llmux.Sidecar would also reap it at VM
# shutdown; being explicit frees the port the moment we are done.
try do
  # 1. The routing table.
  {:ok, 200, body} = Demo.get("#{v1}/models")
  ids = Demo.decode(body)["data"] |> Enum.map(& &1["id"])
  shown = ids |> Enum.take(3) |> Enum.join(", ")
  IO.puts("models  : #{length(ids)} (#{shown}#{if length(ids) > 3, do: ", …", else: ""})")

  # 2. A unary chat completion — the identical JSON the C ABI takes.
  request =
    Demo.encode(%{
      "model" => model,
      "messages" => [%{"role" => "user", "content" => "Say hello in five words."}]
    })

  {:ok, 200, body} = Demo.post("#{v1}/chat/completions", request)
  answer = Demo.decode(body)
  content = answer["choices"] |> hd() |> get_in(["message", "content"])
  IO.puts("chat    : #{String.trim(content)}")

  # 3. The error path. Over HTTP an error is a status code and a JSON body,
  #    where the C ABI hands back a plain string in *err.
  bad =
    Demo.encode(%{
      "model" => "no-such-model-anywhere",
      "messages" => [%{"role" => "user", "content" => "hi"}]
    })

  {:ok, status, body} = Demo.post("#{v1}/chat/completions", bad)
  IO.puts("error   : HTTP #{status} #{String.slice(String.trim(body), 0, 160)}")

  # 4. Crash isolation — the thing a NIF would take away. A process that dies
  #    mid-call takes nothing with it, and the sidecar is untouched.
  {:ok, agent} = Agent.start(fn -> nil end)
  Process.unlink(agent)
  ref = Process.monitor(agent)
  Process.exit(agent, :kill)

  receive do: ({:DOWN, ^ref, :process, _, reason} ->
                 IO.puts(
                   "isolate : a worker died (#{inspect(reason)}); the VM and the sidecar are fine"
                 ))

  {:ok, 200, _} = Demo.get("#{v1}/models")
  IO.puts("          gateway still answering after that")
after
  Llmux.stop()
end

IO.puts("stopped : ok")
