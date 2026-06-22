# frozen_string_literal: true

Gem::Specification.new do |spec|
  spec.name = "llmux"
  spec.version = "0.1.0"
  spec.summary = "The LLM multiplexer, embedded locally — one OpenAI-compatible client for every provider."
  spec.description = "Thin Ruby wrapper that bundles the llmux gateway binary, " \
    "starts it on a local port, and hands your existing OpenAI client a base_url."
  spec.authors = ["llmux"]
  spec.homepage = "https://llmux.to"
  spec.license = "MIT"
  spec.required_ruby_version = ">= 2.7"

  spec.files = Dir["lib/**/*.rb", "bin/**/*", "README.md"]
  spec.require_paths = ["lib"]

  # Convenience client is optional; users add ruby-openai themselves if they
  # want `Llmux.openai`. We do not hard-depend on it.
  spec.metadata = {
    "homepage_uri" => "https://llmux.to",
    "source_code_uri" => "https://github.com/llmux/llmux"
  }
end
