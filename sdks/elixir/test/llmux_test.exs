defmodule LlmuxTest do
  @moduledoc """
  Tests for the llmux Elixir sidecar launcher.

  Run from sdks/elixir:  mix test

  Covers binary resolution, URL formatting, health-poll readiness/timeout,
  singleton/lazy start, cleanup, and an integration test gated on the real
  binary. Most tests drive a fake fixture (a tiny python HTTP server honoring
  LLMUX_ADDR) via the LLMUX_BINARY override, so no real gateway is needed.

  The sidecar is a singleton GenServer; tests run serially (async: false) and
  stop it between cases.
  """
  use ExUnit.Case, async: false

  @fixture Path.expand("fixtures/fake_llmux.py", __DIR__)
  @bundled Path.expand("../priv/bin/llmux", __DIR__)

  setup do
    Llmux.stop()
    on_exit(fn ->
      Llmux.stop()
      System.delete_env("LLMUX_BINARY")
    end)
    :ok
  end

  # --- helpers --------------------------------------------------------------

  defp python do
    Enum.find(["python3", "python"], fn c -> System.find_executable(c) end)
  end

  # Write an executable shell wrapper that runs the python fake fixture.
  defp make_fake(extra_env \\ []) do
    py = python() || flunk("python required for the fake fixture")
    dir = Path.join(System.tmp_dir!(), "llmux-ex-fake-#{:erlang.unique_integer([:positive])}")
    File.mkdir_p!(dir)
    wrapper = Path.join(dir, "llmux")

    exports =
      Enum.map_join(extra_env, "", fn {k, v} -> "export #{k}=\"#{v}\"\n" end)

    File.write!(wrapper, "#!/bin/sh\n#{exports}exec \"#{System.find_executable(py)}\" \"#{@fixture}\"\n")
    File.chmod!(wrapper, 0o755)
    wrapper
  end

  defp port_of(base), do: base |> String.split(":") |> List.last() |> String.to_integer()

  defp port_open?(port) do
    case :gen_tcp.connect(~c"127.0.0.1", port, [:binary, active: false], 300) do
      {:ok, sock} ->
        :gen_tcp.close(sock)
        true

      _ ->
        false
    end
  end

  defp wait_port_closed(port, timeout_ms) do
    deadline = System.monotonic_time(:millisecond) + timeout_ms
    do_wait_port_closed(port, deadline)
  end

  defp do_wait_port_closed(port, deadline) do
    cond do
      not port_open?(port) -> true
      System.monotonic_time(:millisecond) > deadline -> not port_open?(port)
      true ->
        Process.sleep(50)
        do_wait_port_closed(port, deadline)
    end
  end

  defp health_status(base) do
    "http://" <> hostport = base
    [host, port_str] = String.split(hostport, ":", parts: 2)
    port = String.to_integer(port_str)

    case :gen_tcp.connect(String.to_charlist(host), port, [:binary, active: false], 1000) do
      {:ok, sock} ->
        :gen_tcp.send(sock, "GET /health HTTP/1.0\r\nHost: #{hostport}\r\nConnection: close\r\n\r\n")
        result =
          case :gen_tcp.recv(sock, 0, 1000) do
            {:ok, resp} ->
              case Regex.run(~r/HTTP\/1\.\d (\d+)/, resp) do
                [_, code] -> String.to_integer(code)
                _ -> 0
              end

            _ ->
              0
          end

        :gen_tcp.close(sock)
        result

      _ ->
        0
    end
  end

  # --- binary resolution ----------------------------------------------------

  test "LLMUX_BINARY override is used (fake comes up healthy)" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake())
      assert {:ok, base} = Llmux.start()
      assert health_status(base) == 200
    end
  end

  test "clear error when binary missing" do
    System.delete_env("LLMUX_BINARY")

    # Only assert the not-found path when nothing is resolvable.
    if not File.regular?(@bundled) and System.find_executable("llmux") == nil do
      saved_path = System.get_env("PATH")
      System.put_env("PATH", "")

      try do
        assert {:error, reason} = Llmux.start()
        assert reason =~ "llmux binary not found"
      after
        if saved_path, do: System.put_env("PATH", saved_path)
      end
    end
  end

  # --- URL formatting -------------------------------------------------------

  test "openai_base_url appends /v1, base is http://127.0.0.1:<port>" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake())
      assert {:ok, base} = Llmux.base_url()
      assert base =~ ~r{^http://127\.0\.0\.1:\d+$}
      assert {:ok, v1} = Llmux.openai_base_url()
      assert v1 == base <> "/v1"
      assert String.ends_with?(v1, "/v1")
    end
  end

  # --- health-poll logic ----------------------------------------------------

  test "becomes ready when /health returns 200" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake())
      assert {:ok, base} = Llmux.start(timeout: 10_000)
      assert base =~ ~r{^http://127\.0\.0\.1:\d+$}
    end
  end

  test "times out when /health never returns 200" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake([{"FAKE_HEALTH_STATUS", "503"}]))
      assert {:error, reason} = Llmux.start(timeout: 600)
      assert reason =~ "did not become healthy"
    end
  end

  test "times out when the server never listens" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake([{"FAKE_NEVER_LISTEN", "1"}]))
      assert {:error, _reason} = Llmux.start(timeout: 600)
    end
  end

  # --- singleton / lazy start ----------------------------------------------

  test "start twice returns same base, no respawn" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake())
      assert {:ok, b1} = Llmux.start()
      assert {:ok, b2} = Llmux.start()
      assert {:ok, b3} = Llmux.base_url()
      assert b1 == b2
      assert b1 == b3
    end
  end

  # --- cleanup --------------------------------------------------------------

  test "stop kills the child and frees the port" do
    if python() do
      System.put_env("LLMUX_BINARY", make_fake())
      assert {:ok, base} = Llmux.start()
      port = port_of(base)
      assert port_open?(port)
      Llmux.stop()
      assert wait_port_closed(port, 3000), "port should be freed after stop"
    end
  end

  # --- integration (real binary) -------------------------------------------

  @tag :integration
  test "integration: real binary serves health and hands back base_url" do
    real = System.get_env("LLMUX_BINARY_REAL")

    bin =
      cond do
        real not in [nil, ""] -> real
        File.regular?(@bundled) -> @bundled
        true -> nil
      end

    if bin do
      System.put_env("LLMUX_BINARY", bin)
      assert {:ok, base} = Llmux.start(timeout: 15_000)
      assert base =~ ~r{^http://127\.0\.0\.1:\d+$}
      assert health_status(base) == 200
      assert {:ok, v1} = Llmux.openai_base_url()
      assert String.ends_with?(v1, "/v1")
    end
  end
end
