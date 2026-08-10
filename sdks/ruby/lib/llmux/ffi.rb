# frozen_string_literal: true

# llmux in-process, through the C ABI (`libllmux`), using `fiddle` from the
# standard library. No child process, no port, and no gem dependency.
#
#   require "llmux/ffi"
#
#   Llmux::Ffi.open do |llmux|
#     answer = llmux.call("chat", model: "openai/gpt-4o-mini",
#                                 messages: [{ role: "user", content: "hi" }])
#     puts answer.dig("choices", 0, "message", "content")
#   end
#
# READ sdks/ruby/README.md BEFORE YOU USE THIS. The Go runtime ends up inside
# your Ruby process and does not survive fork(), which collides with Unicorn,
# Puma in clustered mode, Resque, and Spring. For those, Llmux (the managed
# sidecar in lib/llmux.rb) is the right answer.
#
# Why `fiddle` and not the `ffi` gem: fiddle ships with Ruby, so the direct mode
# adds no dependency to a gem whose whole selling point is that it is thin.
# Everything needed is there — dlopen, a struct-free calling convention, and
# Fiddle::Closure for the streaming callback.

require "fiddle"
require "json"
require "rbconfig"

module Llmux
  class Error < StandardError; end unless defined?(Error)

  # The C ABI of libllmux (ffi/include/llmux.h) bound with fiddle.
  class Ffi
    PTR = Fiddle::TYPE_VOIDP
    U64 = Fiddle::TYPE_LONG_LONG   # fiddle has no unsigned 64-bit type constant;
    INT = Fiddle::TYPE_INT         # handles are small counters, so this is exact.
    VOID = Fiddle::TYPE_VOID

    # Open a gateway, yield it, and close it however the block exits. This is
    # the form to prefer: `ensure` around a bare .new is the same thing written
    # out longhand, and easier to get wrong.
    #
    #   Llmux::Ffi.open(config: nil) { |llmux| ... }
    def self.open(config: nil, library: nil)
      llmux = new(config: config, library: library)
      begin
        yield llmux
      ensure
        llmux.close
      end
    end

    # config: an llmux configuration document — a Hash, a JSON String, or nil
    #         for built-in defaults plus the environment.
    # library: override library resolution entirely.
    def initialize(config: nil, library: nil)
      @library_path = library || self.class.resolve_library
      begin
        @dl = Fiddle.dlopen(@library_path)
      rescue Fiddle::DLError => e
        raise Error, "could not load #{@library_path}: #{e.message}"
      end

      bind!

      json = case config
             when nil, String then config
             else JSON.generate(config)
             end

      slot = err_slot
      handle = @fn_new.call(json, slot)
      raise Error, "llmux_new: #{take_error(slot)}" if handle.zero?

      @handle = handle
      @mutex = Mutex.new
    end

    # The path this instance actually loaded.
    attr_reader :library_path

    # The llmux version the LOADED library was built from.
    #
    # Worth checking at startup: a shared library is resolved off a load path
    # you may not control, and a stale libllmux earlier on it is called silently
    # otherwise. The returned string is static inside the library — do NOT free.
    def version
      @fn_abi_version.call.to_s
    end

    # One unary call. Methods: "chat", "embed", "models".
    # Returns the parsed JSON response.
    #
    #   llmux.call("chat", model: "…", messages: [...])
    #   llmux.call("models")
    def call(method, request = nil, **kwargs)
      request = kwargs unless kwargs.empty?
      JSON.parse(call_raw(method, request))
    end

    # The same call, returning the response JSON verbatim — the same bytes
    # POST /v1/chat/completions returns.
    def call_raw(method, request = nil)
      assert_open!

      json = case request
             when nil, String then request
             else JSON.generate(request)
             end

      # A FRESH, zeroed slot per call. llmux writes *err on failure only and
      # leaves it untouched otherwise, so a reused slot still holds the previous
      # (already freed) pointer.
      slot = err_slot
      res = @fn_call.call(@handle, method.to_s, json, slot)
      raise Error, "llmux_call(#{method}): #{take_error(slot)}" if res.null?

      begin
        res.to_s   # Fiddle::Pointer#to_s copies up to the NUL into a Ruby String
      ensure
        # Everything the library returns — results AND error messages — is
        # released with llmux_free and nothing else.
        @fn_free.call(res)
      end
    end

    # Stream a chat completion, yielding each `chat.completion.chunk`.
    # Returns the number of chunks delivered.
    #
    #   llmux.stream("chat", model: "…", messages: [...]) do |chunk, raw|
    #     print chunk.dig("choices", 0, "delta", "content").to_s
    #   end
    #
    # Break out early by returning `:stop` or `false` from the block. That stops
    # the stream and is NOT an error — llmux returns success and writes no
    # message, because you already know you asked it to stop.
    #
    # Do not put a bare `break` in the block — see the warning on #stream_enum
    # for why, and return `:stop` instead. Calling `cancel` from inside the
    # block is also safe (measured; see #cancel) and is the ONLY way to reach
    # llmux_cancel from a host with no threads at all; here, where a second
    # Ruby thread can do it too, it is mostly useful when you want the
    # `context canceled` error back from stream() instead of a quiet stop.
    #
    # llmux_cancel is PER HANDLE, not per call: cancelling aborts every call in
    # flight on this gateway, not just this stream. One gateway per
    # cancellation scope if you need them independent.
    #
    # Threads: the callback runs on the thread that called stream, synchronously
    # (asserted in ffi/ctest/smoke.c by comparing pthread ids). Fiddle releases
    # the GVL for the call and Fiddle::Closure reacquires it around your block
    # — see README.md, this was measured rather than assumed.
    def stream(method, request = nil, **kwargs, &block)
      raise ArgumentError, "stream requires a block" unless block

      request = kwargs unless kwargs.empty?
      assert_open!

      json = case request
             when nil, String then request
             else JSON.generate(request)
             end

      count = 0
      escaped = nil

      callback = Fiddle::Closure::BlockCaller.new(INT, [PTR, PTR]) do |chunk_ptr, _user_data|
        if escaped
          1
        else
          count += 1
          begin
            # The header says chunk_json is owned by the library and valid only
            # for this call. Pointer#to_s copies it, so `raw` outlives the frame.
            raw = chunk_ptr.to_s
            verdict = block.call(JSON.parse(raw), raw)
            verdict == false || verdict == :stop ? 1 : 0
          rescue StandardError, ScriptError => e
            # An exception raised here unwinds by longjmp THROUGH a Go call
            # frame, skipping llmux's own cleanup for the stream. It does not
            # crash — measured — but it leaks whatever that frame was going to
            # release. Stop politely instead and re-raise on the way out.
            escaped = e
            1
          end
        end
      end

      slot = err_slot
      rc = @fn_stream.call(@handle, method.to_s, json, callback, nil, slot)

      raise escaped if escaped
      raise Error, "llmux_stream(#{method}): #{take_error(slot)}" unless rc.zero?

      count
    ensure
      # Keep the closure alive until the C call has definitely returned; if the
      # GC freed its trampoline mid-stream, llmux would call into freed memory.
      callback&.free if callback.respond_to?(:free)
    end

    # Stream a chat completion as an Enumerator instead of a block — for
    # `each`, `lazy`, `take_while`, external `#next`, or a plain `for` loop.
    # Abandoning it — `break` out of `each`, or any other early exit — reaches
    # llmux_cancel, same guarantee #stream gives you through its own return
    # value, in the shape Ruby programmers reach for first:
    #
    #   llmux.stream_enum("chat", model: "…", messages: [...]).each do |chunk, raw|
    #     print chunk.dig("choices", 0, "delta", "content")
    #     break if enough?
    #   end
    #
    # Why `break` is safe HERE but a bare `break` inside #stream's block is
    # not: #stream calls your block directly from inside the FFI trampoline,
    # so `break` there would have to unwind past the trampoline and the blocked
    # llmux_stream C frame to reach #stream's own call site — undefined
    # territory, same family of hazard as raising out of the block (see the
    # comment above on that). Here your `break` targets `each`, not `stream`,
    # so the block you actually write never sits inside the trampoline at all:
    # it runs one level further out, inside Enumerator::Yielder#<<, called from
    # the tiny relay block below. Measured on Ruby 4.0.5, against the same
    # fake upstream as #cancel (100 ms/chunk, ten words): breaking after 3
    # chunks ran this method's `ensure` immediately — not on GC, right there as
    # `each` unwound — with the consumer having seen exactly 3 chunks and the
    # upstream's `/generated` never climbing past 3 afterward.
    #
    # That `ensure` calls cancel() unconditionally, including on a stream that
    # ran to completion, where it is a documented no-op — belt and suspenders
    # rather than trusting the unwind above to have already stopped the
    # provider by itself.
    def stream_enum(method, request = nil, **kwargs)
      request = kwargs unless kwargs.empty?
      Enumerator.new do |y|
        stream(method, request) { |chunk, raw| y << [chunk, raw] }
      ensure
        cancel
      end
    end

    # Abort every call in flight on this handle, without closing it: the handle
    # stays open and usable, and the next call starts on a fresh context.
    #
    # This is the ONLY way to get a blocked call() or stream() to return early
    # from outside the call itself. Measured on this machine, against a fake
    # upstream sleeping 100 ms per chunk (`sdks/fake-upstream.py`), cancelling
    # a ten-word stream after 3 delivered chunks:
    #
    #   from a second thread, while the streaming thread was blocked in
    #   @fn_stream.call            -> rc=-1, err="context canceled",
    #                                  consumer saw exactly 3 chunks,
    #                                  cancel-call-to-thread-join: ~1.5 ms
    #
    # It is safe to call from another thread AND from inside the chunk block
    # itself (see #stream) — cancel() takes no lock of its own, deliberately,
    # for the same reason #call and #stream do not: a call in flight must stay
    # reachable while it is in flight. Cancelling an unknown or idle handle is
    # a no-op, so cleanup paths can call it blindly, same as #close.
    def cancel
      return if @handle.nil? || @handle.zero?

      @fn_cancel.call(@handle)
      nil
    end

    # Release the gateway, aborting any stream still running on it. Idempotent,
    # so cleanup paths can call it blindly.
    def close
      @mutex&.synchronize do
        return if @handle.nil? || @handle.zero?

        @fn_close.call(@handle)
        @handle = 0
      end
      nil
    end

    def closed?
      @handle.nil? || @handle.zero?
    end

    # ---------------------------------------------------------------- internals

    private

    def bind!
      # need_gvl: is left at its default (false) on purpose, so fiddle calls
      # rb_thread_call_without_gvl and other Ruby threads keep running during a
      # model call that takes hundreds of milliseconds. Safe for llmux_stream
      # too: fiddle's closure trampoline reacquires the GVL itself
      # (ext/fiddle/closure.c: `if (ruby_thread_has_gvl_p()) … else
      # rb_thread_call_with_gvl(...)`), which is valid precisely because the
      # callback runs on the calling thread. Measured both ways in README.md.
      @fn_abi_version = Fiddle::Function.new(@dl["llmux_abi_version"], [], PTR,
                                             name: "llmux_abi_version")
      @fn_new = Fiddle::Function.new(@dl["llmux_new"], [PTR, PTR], U64, name: "llmux_new")
      @fn_close = Fiddle::Function.new(@dl["llmux_close"], [U64], VOID, name: "llmux_close")
      @fn_call = Fiddle::Function.new(@dl["llmux_call"], [U64, PTR, PTR, PTR], PTR,
                                      name: "llmux_call")
      @fn_free = Fiddle::Function.new(@dl["llmux_free"], [PTR], VOID, name: "llmux_free")
      @fn_stream = Fiddle::Function.new(@dl["llmux_stream"], [U64, PTR, PTR, PTR, PTR, PTR], INT,
                                        name: "llmux_stream")
      @fn_cancel = Fiddle::Function.new(@dl["llmux_cancel"], [U64], VOID, name: "llmux_cancel")
    rescue Fiddle::DLError => e
      raise Error, "#{@library_path} is missing a symbol the ABI promises: #{e.message}"
    end

    def assert_open!
      raise Error, "this Llmux::Ffi handle is closed" if closed?
    end

    # A zeroed `char*` slot for the trailing `char** err` argument.
    def err_slot
      slot = Fiddle::Pointer.malloc(Fiddle::SIZEOF_VOIDP, Fiddle::RUBY_FREE)
      slot[0, Fiddle::SIZEOF_VOIDP] = "\x00".b * Fiddle::SIZEOF_VOIDP
      slot
    end

    # Read the message out of an err slot and free it with llmux_free — an
    # error must not be a leak.
    def take_error(slot)
      ptr = slot.ptr
      return "(no message)" if ptr.null?

      message = ptr.to_s
      @fn_free.call(ptr)
      message
    end

    class << self
      # 1. LLMUX_LIBRARY
      # 2. lib/ inside this gem (where a platform gem would put it)
      # 3. dist/ffi/<goos>_<goarch>/ in a checkout of the llmux repo
      # 4. the bare soname, letting the dynamic loader search its own paths
      def resolve_library
        env = ENV["LLMUX_LIBRARY"]
        return env if env && !env.empty?

        gem_root = File.expand_path("../..", __dir__)  # …/sdks/ruby
        repo = File.expand_path("../..", gem_root)     # …/repo root

        candidates = [
          File.join(gem_root, "lib", library_name),
          File.join(repo, "dist", "ffi", "#{goos}_#{goarch}", library_name)
        ]
        found = candidates.find { |path| File.file?(path) }
        found || library_name
      end

      def library_name
        case RbConfig::CONFIG["host_os"]
        when /mswin|mingw|cygwin/ then "llmux.dll"
        when /darwin/ then "libllmux.dylib"
        else "libllmux.so"
        end
      end

      def goos
        case RbConfig::CONFIG["host_os"]
        when /mswin|mingw|cygwin/ then "windows"
        when /darwin/ then "darwin"
        when /freebsd/ then "freebsd"
        else "linux"
        end
      end

      def goarch
        case RbConfig::CONFIG["host_cpu"]
        when /arm64|aarch64/ then "arm64"
        when /x86_64|amd64/ then "amd64"
        else RbConfig::CONFIG["host_cpu"]
        end
      end
    end
  end
end
