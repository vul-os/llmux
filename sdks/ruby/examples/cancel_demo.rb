#!/usr/bin/env ruby
# frozen_string_literal: true

# llmux_cancel, proven against a upstream that can tell you whether cancelling
# actually stopped it.
#
#   ruby sdks/ruby/examples/cancel_demo.rb
#
# direct_chat.rb's "early" section (#4) shows the OTHER way to stop a stream —
# returning `:stop` from the block — which is answered from #stream()'s own
# return value and never touches the network. That is not what this script is
# about. The question llmux_cancel exists to answer is different: when a
# consumer stops reading after 3 of 10 chunks, did the PROVIDER stop
# generating (and metering) the other 7, or did it run to completion in the
# background while nobody was reading the rest? A callback that just stops
# being called cannot tell you that. `sdks/fake-upstream.py` can — it counts
# chunks it actually writes to the socket and serves that count at
# `GET /generated`, so this script cancels, then goes and asks the upstream
# what it did.
#
# This is a self-contained demo, deliberately not layered onto direct_chat.rb:
# that script's upstream is whatever LLMUX_CONFIG_JSON points at (a real
# provider, or ffi/fakeupstream, neither of which can answer "how many did you
# generate?"), and this measurement needs the counting fake specifically. It
# spawns its own copy on a random port and tears it down on exit.

$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "llmux/ffi"
require "json"
require "net/http"

repo_root = File.expand_path("../../..", __dir__)
fake_upstream = File.join(repo_root, "sdks", "fake-upstream.py")

python = %w[python3 python].find { |c| system("command -v #{c} > /dev/null 2>&1") }
abort "python3 is required to run sdks/fake-upstream.py" unless python

io = IO.popen([python, fake_upstream,
               "--chunk-delay-ms", "100",
               "--text", "one two three four five six seven eight nine ten"])
url = io.gets.to_s[/^URL (\S+)/, 1]
config = io.gets.to_s[/^CONFIG (.+)$/, 1]
abort "fake upstream did not start" unless url && config

begin
  Llmux::Ffi.open(config: config) do |llmux|
    puts "upstream : #{url}"

    # The idiomatic construct: stream_enum() returns a real Enumerator, and
    # breaking out of #each on it reaches llmux_cancel through the Enumerator's
    # own `ensure` — see the comment on Ffi#stream_enum for why that `break` is
    # safe here when a bare `break` inside #stream's own block would not be.
    seen = 0
    print "consumer : "
    llmux.stream_enum("chat", model: "demo",
                       messages: [{ role: "user", content: "hi" }]).each do |chunk, _raw|
      seen += 1
      delta = chunk.dig("choices", 0, "delta", "content")
      print delta if delta
      $stdout.flush
      break if seen >= 3
    end
    puts
    puts "consumer : stopped after #{seen} chunks"

    # Query it AFTER the cancel demo above has finished, per the harness's own
    # contract — this is the number that proves (or disproves) that cancelling
    # actually stopped generation upstream, not just delivery to us.
    generated = JSON.parse(Net::HTTP.get(URI("#{url}/generated")))["generated"]
    puts "upstream : generated #{generated} chunks total " \
         "(a run to completion would be 12 — 10 words plus a finish frame and a usage frame)"

    # The handle survives a cancel: llmux_cancel aborts the call, not the
    # gateway. A plain call on the same handle right afterward should succeed.
    llmux.call("models")
    puts "handle   : still usable after cancel"
  end
ensure
  Process.kill("TERM", io.pid)
  io.close
end
