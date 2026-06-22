defmodule Llmux.Sidecar do
  @moduledoc """
  Singleton GenServer that owns the llmux child process.

  It opens an Erlang `Port` to the bundled binary. We use a small shell wrapper
  so the OS child is reliably killed when the Port closes (the BEAM closes Ports
  on GenServer termination / VM shutdown), keeping the contract's cleanup
  guarantee.
  """

  use GenServer

  defstruct port: nil, os_pid: nil, base: nil

  # --- public API -----------------------------------------------------------

  def start_link(_opts) do
    GenServer.start_link(__MODULE__, %__MODULE__{}, name: __MODULE__)
  end

  @doc "Start the sidecar (idempotent). Returns `{:ok, base_url}`."
  def start(opts \\ []) do
    GenServer.call(__MODULE__, {:start, opts}, 30_000)
  end

  @doc "Stop the sidecar if running."
  def stop do
    GenServer.call(__MODULE__, :stop)
  end

  # --- GenServer ------------------------------------------------------------

  @impl true
  def init(state), do: {:ok, state}

  @impl true
  def handle_call({:start, _opts}, _from, %{port: port} = state) when is_port(port) do
    {:reply, {:ok, state.base}, state}
  end

  def handle_call({:start, opts}, _from, state) do
    port_num = Keyword.get(opts, :port) || free_port()
    addr = "127.0.0.1:#{port_num}"
    base = "http://#{addr}"

    case binary_path() do
      {:ok, bin} ->
        env = build_env(addr, opts)
        port = open_port(bin, env)
        os_pid = port_os_pid(port)

        case wait_healthy(base, Keyword.get(opts, :timeout, 10_000)) do
          :ok ->
            {:reply, {:ok, base}, %{state | port: port, os_pid: os_pid, base: base}}

          {:error, reason} ->
            close(port, os_pid)
            {:reply, {:error, reason}, %__MODULE__{}}
        end

      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  @impl true
  def handle_call(:stop, _from, state) do
    close(state.port, state.os_pid)
    {:reply, :ok, %__MODULE__{}}
  end

  @impl true
  def handle_info({port, {:data, data}}, %{port: port} = state) do
    IO.write(data)
    {:noreply, state}
  end

  def handle_info({port, {:exit_status, _status}}, %{port: port} = state) do
    {:noreply, %__MODULE__{}}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    close(state.port, state.os_pid)
    :ok
  end

  # --- internals ------------------------------------------------------------

  defp open_port(bin, env) do
    Port.open({:spawn_executable, bin}, [
      :binary,
      :exit_status,
      :use_stdio,
      :stderr_to_stdout,
      args: [],
      env: env
    ])
  end

  defp port_os_pid(port) do
    case Port.info(port, :os_pid) do
      {:os_pid, pid} -> pid
      _ -> nil
    end
  end

  defp close(nil, _os_pid), do: :ok

  defp close(port, os_pid) do
    if is_port(port) and Port.info(port) != nil do
      Port.close(port)
    end

    # Belt-and-suspenders: the Port close should reap the child, but make sure.
    if is_integer(os_pid) do
      System.cmd("kill", ["#{os_pid}"], stderr_to_stdout: true)
    end

    :ok
  rescue
    _ -> :ok
  end

  defp build_env(addr, opts) do
    base = [{~c"LLMUX_ADDR", String.to_charlist(addr)}]

    base =
      case Keyword.get(opts, :config) do
        nil -> base
        cfg -> [{~c"LLMUX_CONFIG", String.to_charlist(cfg)} | base]
      end

    extra =
      opts
      |> Keyword.get(:env, [])
      |> Enum.map(fn {k, v} -> {String.to_charlist(to_string(k)), String.to_charlist(to_string(v))} end)

    base ++ extra
  end

  defp binary_path do
    cond do
      (env = System.get_env("LLMUX_BINARY")) not in [nil, ""] ->
        {:ok, env}

      true ->
        name = if match?({:win32, _}, :os.type()), do: "llmux.exe", else: "llmux"
        bundled = Path.join([:code.priv_dir(:llmux) |> to_string_safe(), "bin", name])

        cond do
          File.regular?(bundled) ->
            {:ok, bundled}

          (found = System.find_executable("llmux")) != nil ->
            {:ok, found}

          true ->
            {:error,
             "llmux binary not found. Set LLMUX_BINARY, or build it: " <>
               "`go build -o sdks/elixir/priv/bin/llmux ./cmd/llmux`"}
        end
    end
  end

  # :code.priv_dir returns {:error, :bad_name} before the app is loaded; fall
  # back to a path relative to this module's source location.
  defp to_string_safe({:error, _}), do: Path.join([__DIR__, "..", "..", "priv"])
  defp to_string_safe(dir) when is_list(dir), do: List.to_string(dir)
  defp to_string_safe(dir), do: dir

  defp free_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, ip: {127, 0, 0, 1}, active: false])
    {:ok, port} = :inet.port(socket)
    :gen_tcp.close(socket)
    port
  end

  defp wait_healthy(base, timeout_ms) do
    deadline = System.monotonic_time(:millisecond) + timeout_ms
    do_wait_healthy(base, deadline)
  end

  defp do_wait_healthy(base, deadline) do
    if System.monotonic_time(:millisecond) > deadline do
      {:error, "llmux did not become healthy within the timeout"}
    else
      case health_get(base) do
        :ok ->
          :ok

        :retry ->
          Process.sleep(50)
          do_wait_healthy(base, deadline)
      end
    end
  end

  # Tiny HTTP/1.0 GET /health using :gen_tcp — no HTTP dependency.
  defp health_get("http://" <> hostport) do
    [host, port_str] = String.split(hostport, ":", parts: 2)
    port = String.to_integer(port_str)

    case :gen_tcp.connect(String.to_charlist(host), port, [:binary, active: false], 1000) do
      {:ok, sock} ->
        :gen_tcp.send(sock, "GET /health HTTP/1.0\r\nHost: #{hostport}\r\nConnection: close\r\n\r\n")
        result =
          case :gen_tcp.recv(sock, 0, 1000) do
            {:ok, resp} ->
              if String.starts_with?(resp, "HTTP/1.") and String.contains?(resp, " 200 "),
                do: :ok,
                else: :retry

            _ ->
              :retry
          end

        :gen_tcp.close(sock)
        result

      _ ->
        :retry
    end
  end
end
