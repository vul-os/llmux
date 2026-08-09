#!/usr/bin/env ruby
# frozen_string_literal: true

# SIDECAR (out-of-process) — the SDK spawns `llmux serve` on a loopback port,
# waits for it to be healthy, and shuts it down at exit. You never run a server
# by hand.
#
#   ruby sdks/ruby/examples/sidecar_chat.rb
#
# Environment:
#   LLMUX_BINARY  path to the llmux binary (default: bundled bin/llmux, then PATH)
#   LLMUX_CONFIG  path to an llmux.json (optional)
#   LLMUX_MODEL   model to ask (default: openai/gpt-4o-mini)
#
# This is the shape to use inside Unicorn, clustered Puma, Resque, Sidekiq under
# a forking supervisor, or anything else that forks. sdks/ruby/README.md has the
# measurements behind that.

$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "llmux"
require "json"
require "net/http"
require "uri"

model = ENV.fetch("LLMUX_MODEL", "openai/gpt-4o-mini")

# Starts the child process, waits for GET /health, and registers an at_exit hook
# that terminates it. Idempotent — call it from anywhere.
base = Llmux.start
puts "sidecar : #{base}"

# begin/ensure, so a failure below still stops the child rather than leaving an
# orphaned server holding a port.
begin
  uri = URI("#{Llmux.openai_base_url}/chat/completions")

  Net::HTTP.start(uri.host, uri.port, read_timeout: 120) do |http|
    # 1. The routing table.
    models = JSON.parse(http.get("#{URI(Llmux.openai_base_url).path}/models").body)
    ids = models["data"].map { |m| m["id"] }
    puts "models  : #{ids.size} (#{ids.take(3).join(', ')}#{ids.size > 3 ? ', …' : ''})"

    headers = { "Content-Type" => "application/json",
                "Authorization" => "Bearer llmux-local" }

    # 2. A unary chat completion — the identical JSON the C ABI takes.
    body = JSON.generate(model: model,
                         messages: [{ role: "user", content: "Say hello in five words." }])
    res = http.post(uri.path, body, headers)
    raise "chat: HTTP #{res.code}: #{res.body[0, 200]}" unless res.code == "200"

    puts "chat    : #{JSON.parse(res.body).dig('choices', 0, 'message', 'content').strip}"

    # 3. Streaming, over SSE: `data: {...}` frames terminated by `data: [DONE]`.
    #    These are the same chunk objects llmux_stream hands a callback — the
    #    wire contract is shared between the two modes on purpose.
    print "stream  : "
    $stdout.flush
    chunks = 0
    buffer = +""
    req = Net::HTTP::Post.new(uri.path, headers)
    req.body = JSON.generate(model: model,
                             messages: [{ role: "user", content: "Say hello in five words." }],
                             stream: true)
    http.request(req) do |response|
      response.read_body do |piece|
        buffer << piece
        while (nl = buffer.index("\n"))
          line = buffer.slice!(0, nl + 1).chomp
          next unless line.start_with?("data: ")

          payload = line.delete_prefix("data: ")
          break if payload == "[DONE]"

          chunks += 1
          delta = JSON.parse(payload).dig("choices", 0, "delta", "content")
          next if delta.nil? || delta.empty?

          print delta
          $stdout.flush
        end
      end
    end
    puts "\n          (#{chunks} chunks)"

    # 4. The error path. Over HTTP an error is a status code and a JSON body,
    #    where the C ABI gives you a plain string in *err — the one place the
    #    two modes genuinely differ.
    res = http.post(uri.path,
                    JSON.generate(model: "no-such-model-anywhere",
                                  messages: [{ role: "user", content: "hi" }]),
                    headers)
    puts "error   : HTTP #{res.code} #{res.body.strip[0, 160]}"
  end
ensure
  # The at_exit hook would do this too; doing it here means the port is free the
  # moment we are done with it, on the raise path as well.
  Llmux.stop
end

puts "stopped : ok"
