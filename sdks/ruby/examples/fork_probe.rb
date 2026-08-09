#!/usr/bin/env ruby
# frozen_string_literal: true

# The fork probe — evidence, not a claim.
#
# libllmux puts the Go runtime in your process, and the Go runtime does not
# survive fork() without exec(). Unicorn, clustered Puma, Resque and Spring all
# fork. This makes the collision reproducible on your own machine in about
# thirty seconds:
#
#   ruby sdks/ruby/examples/fork_probe.rb before chat    # expect: HUNG
#   ruby sdks/ruby/examples/fork_probe.rb after  chat    # expect: exited 0
#   ruby sdks/ruby/examples/fork_probe.rb before models  # expect: exited 0  <-- the trap
#
# The third line is the point. `models` is answered from memory and needs
# nothing from the Go scheduler or netpoller, so it works in a child whose
# runtime is already broken, and a health check built on it reports green.
# `chat` opens a socket and hangs forever.
#
# Environment: LLMUX_LIBRARY, LLMUX_CONFIG_JSON, LLMUX_MODEL.

$LOAD_PATH.unshift(File.expand_path("../lib", __dir__))
require "llmux/ffi"

unless Process.respond_to?(:fork)
  warn "this platform has no fork(2) — nothing to probe"
  exit 2
end

when_to_load = ARGV[0] || "before"   # before | after  (relative to the fork)
method = ARGV[1] || "chat"           # chat | models
timeout = (ARGV[2] || "10").to_f

unless %w[before after].include?(when_to_load)
  warn "usage: fork_probe.rb [before|after] [chat|models] [timeout_seconds]"
  exit 2
end

config = ENV["LLMUX_CONFIG_JSON"]
config = nil if config.nil? || config.empty?
model = ENV.fetch("LLMUX_MODEL", "openai/gpt-4o-mini")
request = method == "chat" ? { model: model, messages: [{ role: "user", content: "hi" }] } : nil

# The Unicorn shape: the library is loaded in the master, and every worker is a
# fork() of it. `preload_app true` does exactly this.
parent = when_to_load == "before" ? Llmux::Ffi.new(config: config) : nil

pid = fork do
  # ---- child ("worker") ----------------------------------------------------
  begin
    llmux = parent || Llmux::Ffi.new(config: config)
    begin
      bytes = llmux.call_raw(method, request).bytesize
      warn "  child: #{method} returned #{bytes} bytes"
    ensure
      llmux.close
    end
    exit!(0)
  rescue Llmux::Error => e
    warn "  child: ERROR #{e.message}"
    exit!(1)
  end
end

# ---- parent ("master") -----------------------------------------------------
begin
  deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
  verdict = nil
  while Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline
    reaped, status = Process.waitpid2(pid, Process::WNOHANG)
    if reaped
      verdict = status.exited? ? "exited #{status.exitstatus}" : "killed by signal #{status.termsig}"
      break
    end
    sleep 0.05
  end

  unless verdict
    Process.kill("KILL", pid)
    Process.waitpid(pid)
    verdict = format("HUNG (SIGKILLed after %.0fs)", timeout)
  end

  puts format("load=%-6s method=%-6s -> child %s", when_to_load, method, verdict)
ensure
  parent&.close
end
