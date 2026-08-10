defmodule Llmux.MixProject do
  use Mix.Project

  # Read from the workspace's VERSION file so this package cannot drift
  # from the engine it binds. Six of these manifests sat at 0.1.0 behind a
  # shipped release because each carried its own literal.
  @version Path.join(__DIR__, "../../VERSION") |> File.read!() |> String.trim()

  def project do
    [
      app: :llmux,
      version: @version,
      elixir: "~> 1.12",
      start_permanent: Mix.env() == :prod,
      description:
        "The LLM multiplexer, embedded locally — one OpenAI-compatible client for every provider.",
      package: package(),
      deps: deps()
    ]
  end

  def application do
    # No application callback needed: the sidecar GenServer starts lazily on
    # first use. Provider keys are read from the OS environment.
    [extra_applications: [:logger]]
  end

  defp deps do
    # Core sidecar uses only OTP (Port + :gen_tcp). No runtime deps.
    []
  end

  defp package do
    [
      # The OTP application is still :llmux — renaming the atom would break
      # every Application.get_env(:llmux, ...) call. Only the published Hex
      # name is scoped.
      name: "vulos_llmux",
      licenses: ["MIT"],
      links: %{"Homepage" => "https://llmux.to", "GitHub" => "https://github.com/vul-os/llmux"},
      # priv/bin holds the bundled binary (gitignored; built via `make sdk-bins`).
      files: ~w(lib mix.exs README.md priv)
    ]
  end
end
