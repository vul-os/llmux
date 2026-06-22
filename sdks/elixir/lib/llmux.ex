defmodule Llmux do
  @moduledoc """
  llmux — the LLM multiplexer, embedded locally for Elixir.

  The local wedge: instead of running a server yourself, this starts the gateway
  as a local OS process (via an Erlang `Port`) on `127.0.0.1` and hands you a
  base URL. Point any OpenAI-compatible client at it.

      {:ok, base} = Llmux.base_url()         # "http://127.0.0.1:<port>"
      {:ok, v1} = Llmux.openai_base_url()    # "http://127.0.0.1:<port>/v1"

  Provider keys are inherited from the environment (`OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

  The sidecar is managed by a singleton `GenServer` (`Llmux.Sidecar`). It starts
  lazily on first use and is terminated when the GenServer stops (which the BEAM
  does on shutdown), because the Port is opened with the OS-process-tracking
  options so the child dies with us.
  """

  @doc """
  Start the sidecar (idempotent). Returns `{:ok, base_url}`.

  Options: `:port`, `:config`, `:env` (list of `{key, val}` strings),
  `:timeout` (ms, default 10_000).
  """
  @spec start(keyword()) :: {:ok, String.t()} | {:error, term()}
  def start(opts \\ []) do
    ensure_started()
    Llmux.Sidecar.start(opts)
  end

  @doc "The running base URL, starting the sidecar if needed."
  @spec base_url(keyword()) :: {:ok, String.t()} | {:error, term()}
  def base_url(opts \\ []), do: start(opts)

  @doc "The OpenAI-style base URL (`…/v1`)."
  @spec openai_base_url(keyword()) :: {:ok, String.t()} | {:error, term()}
  def openai_base_url(opts \\ []) do
    with {:ok, base} <- base_url(opts), do: {:ok, base <> "/v1"}
  end

  @doc "Stop the sidecar if running."
  @spec stop() :: :ok
  def stop do
    ensure_started()
    Llmux.Sidecar.stop()
  end

  # Start the singleton GenServer on demand (no application supervision tree
  # required for simple usage; it links to the calling process's group leader).
  defp ensure_started do
    case Process.whereis(Llmux.Sidecar) do
      nil ->
        case Llmux.Sidecar.start_link([]) do
          {:ok, _pid} -> :ok
          {:error, {:already_started, _pid}} -> :ok
          other -> other
        end

      _pid ->
        :ok
    end
  end
end
