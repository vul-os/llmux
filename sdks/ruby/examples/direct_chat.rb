#!/usr/bin/env ruby
# frozen_string_literal: true

# DIRECT (in-process) — llmux through the C ABI, no child process and no port.
#
#   ruby sdks/ruby/examples/direct_chat.rb
#
# Environment:
#   LLMUX_LIBRARY      path to libllmux.dylib/.so (default: see Ffi.resolve_library)
#   LLMUX_CONFIG_JSON  an llmux config document, inline. Unset means built-in
#                      defaults plus the environment (OPENAI_API_KEY, ...).
#   LLMUX_MODEL        model to ask (default: openai/gpt-4o-mini)
#
# Before you put this in a Rails app, read sdks/ruby/README.md: Unicorn and
# clustered Puma fork their workers, and the Go runtime inside libllmux does not
# survive fork(). examples/sidecar_chat.rb is the shape for those.

$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "llmux/ffi"

config = ENV["LLMUX_CONFIG_JSON"]
config = nil if config.nil? || config.empty?
model = ENV.fetch("LLMUX_MODEL", "openai/gpt-4o-mini")

# Ffi.open closes the handle however the block exits — a raise, a return, or a
# normal finish. It is `ensure` written once instead of at every call site.
Llmux::Ffi.open(config: config) do |llmux|
  puts "library : #{llmux.library_path}"
  puts "abi     : #{llmux.version}"

  # 1. A call that touches no upstream: the routing table itself.
  ids = llmux.call("models")["data"].map { |m| m["id"] }
  puts "models  : #{ids.size} (#{ids.take(3).join(', ')}#{ids.size > 3 ? ', …' : ''})"

  # 2. A unary chat completion.
  answer = llmux.call("chat",
                      model: model,
                      messages: [{ role: "user", content: "Say hello in five words." }])
  puts "chat    : #{answer.dig('choices', 0, 'message', 'content').strip}"

  # 3. The same request, streamed. The block runs on THIS thread, synchronously,
  #    before stream returns.
  print "stream  : "
  $stdout.flush
  chunks = llmux.stream("chat",
                        model: model,
                        messages: [{ role: "user", content: "Say hello in five words." }]) do |chunk, _raw|
    delta = chunk.dig("choices", 0, "delta", "content")
    next if delta.nil? || delta.empty?

    print delta
    $stdout.flush
  end
  puts "\n          (#{chunks} chunks)"

  # 4. Stopping early. Returning :stop stops the stream and is NOT an error.
  seen = 0
  llmux.stream("chat",
               model: model,
               messages: [{ role: "user", content: "Count to fifty." }]) do
    seen += 1
    seen >= 3 ? :stop : nil
  end
  puts "early   : stopped after #{seen} chunks, no error"

  # 5. The error path. The message llmux wrote to *err is freed with llmux_free
  #    inside call_raw before the exception is raised — an error is not a leak.
  begin
    llmux.call("summon-a-demon", {})
    puts "error   : UNEXPECTED — an unknown method should fail"
  rescue Llmux::Error => e
    puts "error   : #{e.message}"
  end

  # 6. What the GVL choice buys. fiddle releases the GVL for the call, so other
  #    Ruby threads keep running while llmux is busy.
  ticks = 0
  stop = false
  worker = Thread.new { ticks += 1 until stop }
  llmux.call("chat", model: model, messages: [{ role: "user", content: "hi" }])
  stop = true
  worker.join
  puts "threads : a background Ruby thread ran #{ticks} times during one call"
end

puts "closed  : ok"
