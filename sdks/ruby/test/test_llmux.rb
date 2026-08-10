# frozen_string_literal: true

# Tests for the llmux Ruby sidecar launcher.
# Run from sdks/ruby:  ruby -Ilib -Itest test/test_llmux.rb
#
# Covers: binary resolution, URL formatting, health-poll readiness/timeout,
# singleton/lazy start, cleanup, an integration test gated on the real binary
# (LLMUX_BINARY or the bundled bin/llmux), and direct-mode llmux_cancel tests
# gated on a real libllmux (LLMUX_LIBRARY or dist/ffi/<goos>_<goarch>/), run
# against sdks/fake-upstream.py.

require "minitest/autorun"
require "socket"
require "fileutils"
require "tmpdir"
require "rbconfig"
require "net/http"
require "json"
require "llmux"

RUBY = RbConfig.ruby
FAKE = File.expand_path("fixtures/fake_llmux.rb", __dir__)
BUNDLED = File.expand_path("../bin/llmux", __dir__)

module FakeBin
  module_function

  # Write an executable shell wrapper that runs the fake fixture under ruby.
  def make(extra_env = {})
    dir = Dir.mktmpdir("llmux-fake-")
    wrapper = File.join(dir, "llmux")
    exports = extra_env.map { |k, v| %(export #{k}="#{v}") }.join("\n")
    File.write(wrapper, <<~SH)
      #!/bin/sh
      #{exports}
      exec "#{RUBY}" "#{FAKE}"
    SH
    File.chmod(0o755, wrapper)
    wrapper
  end
end

module PortHelpers
  module_function

  def port_open?(port)
    TCPSocket.new("127.0.0.1", port).close
    true
  rescue StandardError
    false
  end

  def wait_port_closed(port, timeout)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
    sleep 0.05 while port_open?(port) && Process.clock_gettime(Process::CLOCK_MONOTONIC) < deadline
    !port_open?(port)
  end
end

class LlmuxTestBase < Minitest::Test
  def setup
    reset_singleton
    @saved_env = ENV.to_h
  end

  def teardown
    reset_singleton
    ENV.replace(@saved_env)
  end

  def reset_singleton
    Llmux.stop
    Llmux.instance_variable_set(:@pid, nil)
    Llmux.instance_variable_set(:@base, nil)
  end
end

class BinaryResolutionTest < LlmuxTestBase
  def test_env_override_wins
    Dir.mktmpdir do |d|
      target = File.join(d, "custom-llmux")
      File.write(target, "#!/bin/sh\n")
      ENV["LLMUX_BINARY"] = target
      assert_equal target, Llmux.send(:binary_path)
    end
  end

  def test_falls_back_to_path
    Dir.mktmpdir do |d|
      ENV.delete("LLMUX_BINARY")
      tool = File.join(d, "llmux")
      File.write(tool, "#!/bin/sh\n")
      File.chmod(0o755, tool)
      ENV["PATH"] = d
      without_bundled_bin do
        assert_equal tool, Llmux.send(:binary_path)
      end
    end
  end

  def test_clear_error_when_missing
    ENV.delete("LLMUX_BINARY")
    ENV["PATH"] = ""
    without_bundled_bin do
      err = assert_raises(Llmux::Error) { Llmux.send(:binary_path) }
      assert_includes err.message, "llmux binary not found"
    end
  end

  # Hide the bundled bin/llmux for the duration of the block by stubbing
  # File.exist? for that one path.
  def without_bundled_bin
    orig = File.method(:exist?)
    bundled = File.expand_path(BUNDLED)
    File.singleton_class.send(:define_method, :exist?) do |p|
      next false if File.expand_path(p.to_s) == bundled

      orig.call(p)
    end
    yield
  ensure
    File.singleton_class.send(:define_method, :exist?, orig)
  end
end

class UrlFormattingTest < LlmuxTestBase
  include PortHelpers

  def test_openai_base_url_appends_v1
    ENV["LLMUX_BINARY"] = FakeBin.make
    base = Llmux.base_url
    assert_match(%r{\Ahttp://127\.0\.0\.1:\d+\z}, base)
    assert_equal "#{base}/v1", Llmux.openai_base_url
    assert Llmux.openai_base_url.end_with?("/v1")
  end
end

class HealthPollTest < LlmuxTestBase
  def test_becomes_ready_on_200
    ENV["LLMUX_BINARY"] = FakeBin.make
    base = Llmux.start(timeout: 10.0)
    assert_match(%r{\Ahttp://127\.0\.0\.1:\d+\z}, base)
  end

  def test_times_out_when_never_200
    ENV["LLMUX_BINARY"] = FakeBin.make("FAKE_HEALTH_STATUS" => "503")
    err = assert_raises(Llmux::Error) { Llmux.start(timeout: 0.6) }
    assert_includes err.message, "did not become healthy"
    assert_nil Llmux.instance_variable_get(:@pid)
  end

  def test_times_out_when_unreachable
    ENV["LLMUX_BINARY"] = FakeBin.make("FAKE_NEVER_LISTEN" => "1")
    assert_raises(Llmux::Error) { Llmux.start(timeout: 0.6) }
  end

  def test_wait_healthy_helper_directly
    server = TCPServer.new("127.0.0.1", 0)
    port = server.addr[1]
    t = Thread.new do
      loop do
        conn = server.accept
        conn.gets
        conn.write "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        conn.close
      rescue StandardError
        break
      end
    end
    begin
      Llmux.send(:wait_healthy, "http://127.0.0.1:#{port}", 3.0) # should not raise
    ensure
      server.close
      t.kill
    end
  end
end

class SingletonTest < LlmuxTestBase
  def test_start_twice_same_base_no_respawn
    ENV["LLMUX_BINARY"] = FakeBin.make
    b1 = Llmux.start
    pid1 = Llmux.instance_variable_get(:@pid)
    b2 = Llmux.start
    b3 = Llmux.base_url
    assert_equal b1, b2
    assert_equal b1, b3
    assert_equal pid1, Llmux.instance_variable_get(:@pid)
  end
end

class CleanupTest < LlmuxTestBase
  include PortHelpers

  def test_stop_kills_child_and_frees_port
    ENV["LLMUX_BINARY"] = FakeBin.make
    base = Llmux.start
    port = base.split(":").last.to_i
    assert port_open?(port), "port should be open while running"
    Llmux.stop
    assert_nil Llmux.instance_variable_get(:@pid)
    assert wait_port_closed(port, 3.0), "port should be freed after stop"
  end
end

class DirectModeCancelTest < LlmuxTestBase
  FAKE_UPSTREAM = File.expand_path("../../fake-upstream.py", __dir__)

  # Spawn sdks/fake-upstream.py and block until it has printed its first two
  # lines (URL, CONFIG), same contract the SDKs' own examples rely on.
  def with_fake_upstream(chunk_delay_ms:, text:)
    python = %w[python3 python].find { |c| system("command -v #{c} > /dev/null 2>&1") }
    skip "python3 required for sdks/fake-upstream.py" unless python

    io = IO.popen([python, FAKE_UPSTREAM, "--chunk-delay-ms", chunk_delay_ms.to_s, "--text", text])
    url = io.gets.to_s[/^URL (\S+)/, 1]
    config = io.gets.to_s[/^CONFIG (.+)$/, 1]
    raise "fake upstream did not start" unless url && config

    yield url, config
  ensure
    if io
      Process.kill("TERM", io.pid)
      io.close
    end
  end

  def generated_count(base_url)
    JSON.parse(Net::HTTP.get(URI("#{base_url}/generated")))["generated"]
  end

  # This is the regression test for the bug llmux_cancel exists to fix: a
  # consumer that stops reading after N chunks must not leave the upstream
  # generating (and metering) the rest in the background. See
  # sdks/ruby/README.md's "Cancellation" section for the numbers this
  # reproduces on this machine.
  def test_stream_enum_break_reaches_llmux_cancel
    require "llmux/ffi"
    library = ENV["LLMUX_LIBRARY"] || Llmux::Ffi.resolve_library
    skip "no libllmux available (set LLMUX_LIBRARY or run scripts/build-ffi.sh)" unless File.file?(library)

    with_fake_upstream(chunk_delay_ms: 20, text: "one two three four five six seven eight nine ten") do |url, config|
      Llmux::Ffi.open(config: config, library: library) do |llmux|
        seen = 0
        llmux.stream_enum("chat", model: "demo",
                           messages: [{ role: "user", content: "hi" }]).each do |_chunk, _raw|
          seen += 1
          break if seen >= 3
        end
        assert_equal 3, seen, "the consumer should see exactly the chunks generated before it broke"

        # Give the cancelled connection a moment to be observed upstream, then
        # confirm generation actually stopped instead of finishing in the
        # background unread. 12 is "one" through "ten" plus the finish and
        # usage chunks — see fake-upstream.py and sdks/ruby/README.md.
        sleep 0.2
        generated = generated_count(url)
        assert_equal 3, generated,
          "llmux_cancel should have stopped the upstream at 3 chunks, not let it run to #{generated}"

        # The handle must survive: llmux_cancel aborts the call, not the gateway.
        assert llmux.call("models").key?("data")
      end
    end
  end

  def test_cancel_from_another_thread_stops_a_blocked_stream
    require "llmux/ffi"
    library = ENV["LLMUX_LIBRARY"] || Llmux::Ffi.resolve_library
    skip "no libllmux available (set LLMUX_LIBRARY or run scripts/build-ffi.sh)" unless File.file?(library)

    with_fake_upstream(chunk_delay_ms: 20, text: "one two three four five six seven eight nine ten") do |url, config|
      Llmux::Ffi.open(config: config, library: library) do |llmux|
        seen = 0
        signal = Queue.new
        rc_error = nil

        streamer = Thread.new do
          llmux.stream("chat", model: "demo", messages: [{ role: "user", content: "hi" }]) do |_c, _r|
            seen += 1
            signal << true if seen == 3
            0
          end
        rescue Llmux::Error => e
          rc_error = e
        end

        signal.pop
        llmux.cancel
        streamer.join(5)

        assert_equal 3, seen
        assert rc_error, "a stream cancelled mid-flight should return the context-canceled error"
        assert_includes rc_error.message, "context canceled"
      end
    end
  end
end

class IntegrationTest < LlmuxTestBase
  def real_binary
    ENV["LLMUX_BINARY"] || (File.exist?(BUNDLED) ? BUNDLED : nil)
  end

  def test_end_to_end
    bin = @saved_env["LLMUX_BINARY"] || (File.exist?(BUNDLED) ? BUNDLED : nil)
    skip "real llmux binary not available" unless bin

    ENV["LLMUX_BINARY"] = bin
    base = Llmux.start(timeout: 15.0)
    assert_match(%r{\Ahttp://127\.0\.0\.1:\d+\z}, base)
    require "net/http"
    res = Net::HTTP.get_response(URI("#{base}/health"))
    assert_equal "200", res.code
    assert Llmux.openai_base_url.end_with?("/v1")
  end
end
