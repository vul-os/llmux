# frozen_string_literal: true

# llmux — the LLM multiplexer, embedded locally for Ruby.
#
#   require "llmux"
#   client = Llmux.openai      # spawns the gateway, returns a ruby-openai client
#   resp = client.chat(parameters: {
#     model: "anthropic/claude-3-5-sonnet",
#     messages: [{ role: "user", content: "hi" }],
#   })
#
# No server to run: the gateway starts as a local child process and your
# existing OpenAI client points at it. Provider keys come from env vars
# (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, ...).

require "socket"
require "net/http"
require "uri"
require "rbconfig"

module Llmux
  VERSION = "0.1.0"

  class Error < StandardError; end

  @mutex = Mutex.new
  @pid = nil
  @base = nil

  class << self
    # Start the sidecar (idempotent). Returns the base URL (http://host:port).
    #
    # Provider API keys are inherited from the environment (OPENAI_API_KEY,
    # etc.), so the gateway auto-detects providers like the standalone binary.
    def start(port: nil, config: nil, env: nil, timeout: 10.0)
      @mutex.synchronize do
        return @base if running?

        port ||= free_port
        addr = "127.0.0.1:#{port}"
        child_env = { "LLMUX_ADDR" => addr }
        child_env["LLMUX_CONFIG"] = config if config
        child_env.merge!(env) if env

        @pid = Process.spawn(child_env, binary_path, in: :in, out: :out, err: :err)
        @base = "http://#{addr}"
        begin
          wait_healthy(@base, timeout)
        rescue StandardError
          stop_locked
          raise
        end
        at_exit { stop }
        @base
      end
    end

    # The running base URL (http://host:port), starting the sidecar if needed.
    def base_url
      return @base if running?

      start
    end

    # The OpenAI-style base URL (…/v1) for SDK base_uri arguments.
    def openai_base_url
      "#{base_url}/v1"
    end

    # Stop the sidecar if running.
    def stop
      @mutex.synchronize { stop_locked }
    end

    # Return a `ruby-openai` client pointed at the local gateway.
    # Requires the optional `ruby-openai` gem.
    def openai(access_token: "llmux-local", **kwargs)
      require "openai"
      uri = openai_base_url
      # ruby-openai expects the base without the trailing /v1 path; it appends
      # the API version path itself. Strip it back to the bare base.
      ::OpenAI::Client.new(
        access_token: access_token,
        uri_base: base_url,
        **kwargs
      )
    end

    private

    def running?
      !@pid.nil? && process_alive?(@pid)
    end

    def process_alive?(pid)
      Process.kill(0, pid)
      true
    rescue Errno::ESRCH, Errno::EPERM
      false
    end

    def stop_locked
      if @pid && process_alive?(@pid)
        begin
          Process.kill("TERM", @pid)
          deadline = monotonic + 5.0
          Process.wait(@pid, Process::WNOHANG)
          while process_alive?(@pid) && monotonic < deadline
            sleep 0.05
            Process.wait(@pid, Process::WNOHANG)
          end
          Process.kill("KILL", @pid) if process_alive?(@pid)
        rescue Errno::ESRCH, Errno::ECHILD
          # already gone
        end
      end
      @pid = nil
    end

    def binary_path
      # 1) explicit override
      env = ENV["LLMUX_BINARY"]
      return env if env && !env.empty?

      # 2) binary bundled in the gem
      name = windows? ? "llmux.exe" : "llmux"
      bundled = File.join(__dir__, "..", "bin", name)
      return File.expand_path(bundled) if File.exist?(bundled)

      # 3) on PATH
      path = which("llmux")
      return path if path

      raise Error, "llmux binary not found. Set LLMUX_BINARY, install a " \
        "platform gem, or build it: " \
        "`go build -o sdks/ruby/bin/llmux ./cmd/llmux`"
    end

    def which(cmd)
      exts = windows? ? ENV.fetch("PATHEXT", "").split(";") : [""]
      ENV.fetch("PATH", "").split(File::PATH_SEPARATOR).each do |dir|
        exts.each do |ext|
          candidate = File.join(dir, "#{cmd}#{ext}")
          return candidate if File.executable?(candidate) && !File.directory?(candidate)
        end
      end
      nil
    end

    def windows?
      RbConfig::CONFIG["host_os"] =~ /mswin|mingw|cygwin/
    end

    def free_port
      server = TCPServer.new("127.0.0.1", 0)
      port = server.addr[1]
      server.close
      port
    end

    def wait_healthy(base, timeout)
      deadline = monotonic + timeout
      last = nil
      uri = URI("#{base}/health")
      while monotonic < deadline
        begin
          res = Net::HTTP.start(uri.host, uri.port, open_timeout: 1, read_timeout: 1) do |http|
            http.get(uri.request_uri)
          end
          return if res.code.to_i == 200
        rescue StandardError => e
          last = e
        end
        sleep 0.05
      end
      raise Error, "llmux did not become healthy within #{timeout}s: #{last}"
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end
  end
end
