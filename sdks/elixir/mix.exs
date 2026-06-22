defmodule Llmux.MixProject do
  use Mix.Project

  def project do
    [
      app: :llmux,
      version: "0.1.0",
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
      licenses: ["MIT"],
      links: %{"Homepage" => "https://llmux.to", "GitHub" => "https://github.com/llmux/llmux"},
      # priv/bin holds the bundled binary (gitignored; built via `make sdk-bins`).
      files: ~w(lib mix.exs README.md priv)
    ]
  end
end
