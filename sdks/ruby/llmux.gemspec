# frozen_string_literal: true

Gem::Specification.new do |spec|
  # Published as vulos-llmux; `require "llmux"` is unchanged.
  spec.name = "vulos-llmux"
  spec.version = "0.1.7"
  spec.summary = "The LLM multiplexer, embedded locally — one OpenAI-compatible client for every provider."
  spec.description = "Thin Ruby wrapper that bundles the llmux gateway binary, " \
    "starts it on a local port, and hands your existing OpenAI client a base_url."
  spec.authors = ["llmux"]
  spec.homepage = "https://llmux.to"
  spec.license = "MIT"
  spec.required_ruby_version = ">= 2.7"

  spec.files = Dir["lib/**/*.rb", "bin/**/*", "examples/**/*.rb", "README.md"]
  spec.require_paths = ["lib"]

  # `require "llmux/ffi"` (the in-process C-ABI mode) uses fiddle, which is a
  # DEFAULT gem — present in every supported Ruby without being declared. It is
  # listed here only so a Gemfile.lock records the version actually used; no
  # third-party FFI binding is pulled in.
  spec.add_dependency "fiddle", ">= 1.0"

  # Convenience client is optional; users add ruby-openai themselves if they
  # want `Llmux.openai`. We do not hard-depend on it.
  spec.metadata = {
    "homepage_uri" => "https://llmux.to",
    "source_code_uri" => "https://github.com/vul-os/llmux"
  }
end
