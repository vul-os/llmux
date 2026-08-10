<?php

declare(strict_types=1);

namespace Llmux;

/**
 * llmux in-process, through the C ABI (`libllmux`), using PHP's bundled FFI
 * extension. No child process, no port.
 *
 *   $llmux = new \Llmux\Ffi();               // built-in defaults + environment
 *   try {
 *       $answer = $llmux->call('chat', [
 *           'model'    => 'openai/gpt-4o-mini',
 *           'messages' => [['role' => 'user', 'content' => 'hi']],
 *       ]);
 *   } finally {
 *       $llmux->close();                     // frees the handle, always
 *   }
 *
 * READ sdks/php/README.md BEFORE YOU USE THIS. In one line: the Go runtime
 * ends up inside your PHP process and it is not fork-safe, which is a direct
 * collision with php-fpm. For most PHP deployments \Llmux\Llmux — the managed
 * sidecar — is the right answer, and the README says so with measurements.
 *
 * Requirements: ext-ffi (bundled with PHP >= 7.4, but see `ffi.enable`) and a
 * libllmux built for this platform.
 */
final class Ffi
{
    /**
     * The seven functions of the C ABI, transcribed from ffi/include/llmux.h.
     * PHP's FFI parser does not run the C preprocessor, so the header itself
     * (with its #ifndef and #include) cannot be handed to FFI::cdef.
     */
    private const CDEF = <<<'C'
        const char* llmux_abi_version(void);
        uint64_t llmux_new(const char* config_json, char** err);
        void llmux_close(uint64_t h);
        void llmux_cancel(uint64_t h);
        char* llmux_call(uint64_t h, const char* method, const char* request_json, char** err);
        void llmux_free(char* p);
        typedef int (*llmux_chunk_cb)(const char* chunk_json, void* user_data);
        int llmux_stream(uint64_t h, const char* method, const char* request_json,
                         llmux_chunk_cb cb, void* user_data, char** err);
        C;

    /** @var \FFI */
    private $ffi;

    /** @var int */
    private $handle = 0;

    /** @var string */
    private $libraryPath;

    /**
     * Open a gateway.
     *
     * @param array<string,mixed>|string|null $config an llmux configuration
     *        document — the same JSON `llmux serve` reads from llmux.json.
     *        null means built-in defaults plus the environment.
     * @param string|null $libraryPath override library resolution entirely.
     */
    public function __construct($config = null, ?string $libraryPath = null)
    {
        if (!\extension_loaded('FFI')) {
            throw new LlmuxException(
                'the FFI extension is not loaded. It ships with PHP >= 7.4 but is ' .
                'often disabled: enable extension=ffi in php.ini. See sdks/php/README.md.'
            );
        }

        $this->libraryPath = $libraryPath ?? self::resolveLibrary();

        try {
            $this->ffi = \FFI::cdef(self::CDEF, $this->libraryPath);
        } catch (\FFI\Exception $e) {
            // The overwhelmingly common cause outside the CLI SAPI.
            throw new LlmuxException(
                'FFI::cdef failed for ' . $this->libraryPath . ': ' . $e->getMessage() .
                ($this->looksRestricted($e)
                    ? "\nffi.enable is 'preload' by default, which forbids FFI::cdef in " .
                      'any SAPI except CLI. See sdks/php/README.md — the sidecar ' .
                      '(\\Llmux\\Llmux) needs no php.ini change at all.'
                    : ''),
                0,
                $e
            );
        }

        $json = \is_string($config) || $config === null
            ? $config
            : self::encode($config);

        $err = $this->ffi->new('char*');
        $handle = $this->ffi->llmux_new($json, \FFI::addr($err));
        if ($handle === 0) {
            throw new LlmuxException('llmux_new: ' . $this->takeError($err));
        }
        $this->handle = (int) $handle;
    }

    /**
     * Run $fn with an open gateway and close it afterwards, whatever happens.
     * PHP has no `using`/`with`, so this is the closest idiomatic construct;
     * try/finally around a plain constructor is equally fine.
     *
     * @param array<string,mixed>|string|null $config
     * @return mixed whatever $fn returns
     */
    public static function with($config, callable $fn, ?string $libraryPath = null)
    {
        $llmux = new self($config, $libraryPath);
        try {
            return $fn($llmux);
        } finally {
            $llmux->close();
        }
    }

    /**
     * The llmux version the LOADED library was built from.
     *
     * Compare it against what your code expects: a shared library is resolved
     * off a load path you may not control, and without this probe a stale
     * libllmux earlier on that path is called silently.
     */
    public function version(): string
    {
        // llmux_abi_version returns `const char*`; PHP FFI converts a const
        // char* return straight to a PHP string. It is a static string in the
        // library and must NOT be freed.
        return (string) $this->ffi->llmux_abi_version();
    }

    /** The path this instance actually loaded. */
    public function libraryPath(): string
    {
        return $this->libraryPath;
    }

    /**
     * One unary call. Methods: "chat", "embed", "models".
     *
     * @param array<string,mixed>|string|null $request
     * @return array<string,mixed> the decoded JSON response
     */
    public function call(string $method, $request = null): array
    {
        $raw = $this->callRaw($method, $request);
        $decoded = \json_decode($raw, true);
        if (!\is_array($decoded)) {
            throw new LlmuxException("llmux returned a non-object response to '{$method}'");
        }

        return $decoded;
    }

    /**
     * One unary call, returning the response JSON verbatim. Use this when you
     * want to hand the bytes to something else without a decode/encode round
     * trip — it is the same JSON `POST /v1/chat/completions` returns.
     *
     * @param array<string,mixed>|string|null $request
     */
    public function callRaw(string $method, $request = null): string
    {
        $this->assertOpen();

        $json = \is_string($request) || $request === null ? $request : self::encode($request);

        // A FRESH slot per call, deliberately. llmux writes *err on failure
        // only and leaves it untouched otherwise, so a reused slot still holds
        // the previous (already-freed) pointer. FFI::new zeroes its allocation.
        $err = $this->ffi->new('char*');
        $res = $this->ffi->llmux_call($this->handle, $method, $json, \FFI::addr($err));

        // A NULL `char*` return arrives in PHP as null, not as a null CData.
        if ($res === null) {
            throw new LlmuxException("llmux_call({$method}): " . $this->takeError($err));
        }

        try {
            return \FFI::string($res);
        } finally {
            $this->ffi->llmux_free($res);
        }
    }

    /**
     * Stream a chat completion. $onChunk is called once per
     * `chat.completion.chunk`, on THIS thread, before stream() returns.
     * Return false (or a non-zero int) from $onChunk to stop early — that is
     * not an error, and (measured, see README.md's Cancellation section) it
     * already stops the provider too, not just delivery to your callback.
     *
     * Calling cancel() from inside $onChunk also works (measured) and is the
     * ONLY way to reach llmux_cancel from a plain callback-based stream():
     * PHP's common CLI/FPM builds have no threads, so there is no "call it
     * from another thread" option the way there is in Ffi::cancel()'s own doc
     * comment for languages that have one. The difference from returning
     * false is the return value: cancel() makes stream() throw with
     * "context canceled" instead of returning quietly.
     *
     * @param array<string,mixed>|string $request
     * @param callable(array<string,mixed>,string):mixed $onChunk receives the
     *        decoded chunk and the raw chunk JSON.
     * @return int the number of chunks delivered
     */
    public function stream(string $method, $request, callable $onChunk): int
    {
        $this->assertOpen();

        $json = \is_string($request) ? $request : self::encode($request);

        $count = 0;
        /** @var \Throwable|null $escaped */
        $escaped = null;

        // Throwing out of an FFI callback is a FATAL error in PHP — not a
        // catchable one ("Throwing from FFI callbacks is not allowed"). So the
        // callback swallows everything, asks llmux to stop, and we rethrow on
        // this side once the Go call frame has unwound.
        $trampoline = function ($chunkJson, $userData) use (&$count, &$escaped, $onChunk): int {
            if ($escaped !== null) {
                return 1;
            }
            // A `const char*` parameter arrives in a PHP callback as a PHP
            // string: already copied, so it outlives the callback safely.
            $raw = \is_string($chunkJson) ? $chunkJson : \FFI::string($chunkJson);
            $count++;
            try {
                $decoded = \json_decode($raw, true);
                $verdict = $onChunk(\is_array($decoded) ? $decoded : [], $raw);
            } catch (\Throwable $t) {
                $escaped = $t;

                return 1;
            }

            if ($verdict === false) {
                return 1;
            }

            return \is_int($verdict) ? $verdict : 0;
        };

        $err = $this->ffi->new('char*');
        $rc = $this->ffi->llmux_stream(
            $this->handle,
            $method,
            $json,
            $trampoline,
            null,
            \FFI::addr($err)
        );

        if ($escaped !== null) {
            // Nothing was written to *err (we stopped the stream ourselves), so
            // there is nothing to free here.
            throw $escaped;
        }
        if ($rc !== 0) {
            throw new LlmuxException("llmux_stream({$method}): " . $this->takeError($err));
        }

        return $count;
    }

    /**
     * Stream a chat completion as a Generator instead of a callback — for
     * `foreach`, `iterator_to_array()`, or anywhere PHP's own iteration
     * protocol fits better than passing in a function. Abandoning it —
     * `break` out of the foreach, `return` out of the loop, an exception that
     * skips past it — reaches llmux_cancel, which is what a PHP developer
     * expects `foreach ... break` to do and what stream()'s plain callback
     * form cannot give you (see the warning on stream()).
     *
     *   foreach ($llmux->streamGenerator('chat', $request) as [$chunk, $raw]) {
     *       echo $chunk['choices'][0]['delta']['content'] ?? '';
     *       if ($enough) {
     *           break;
     *       }
     *   }
     *
     * llmux_stream is one blocking call that invokes its callback
     * synchronously and does not return until the stream ends or the callback
     * says stop — there is no native way to pause it mid-flight and hand a
     * value to a `yield`, because a `yield` only works lexically inside the
     * generator function itself, never inside a nested closure. This bridges
     * the two with a Fiber (PHP >= 8.1): the blocking llmux_stream call runs
     * INSIDE the fiber, its C callback suspends the fiber with each chunk, and
     * this method resumes the fiber once per value it yields out. Suspending a
     * fiber parks its entire call stack, not just PHP-level bookkeeping, so
     * what actually gets "paused" between yields is the whole native call —
     * the FFI trampoline and the blocked llmux_stream frame underneath it —
     * still open, still holding the connection, exactly as if nobody had
     * touched it.
     *
     * That parking is also why `break` is safe HERE in a way it is not inside
     * stream()'s own callback: the code in your foreach body runs in THIS
     * generator function, one level outside the fiber — `break` only has to
     * unwind this function's own `finally`, never the FFI trampoline or the
     * Go call frame the way a bare `break` placed directly inside stream()'s
     * callback would have to.
     *
     * Measured on PHP 8.5.9 against sdks/fake-upstream.py (100 ms/chunk, ten
     * words): breaking a foreach after 3 chunks ran the `finally` below
     * immediately, in the same request that broke the loop — PHP runs a
     * generator's pending `finally` as soon as the generator becomes
     * unreachable, which for a `foreach` target with no other reference is
     * the moment the loop exits, not "eventually, whenever the garbage
     * collector gets to it." cancel()-to-fiber-terminated took under 1 ms, and
     * the upstream's own `/generated` read exactly 3 afterward — see
     * README.md's Cancellation section for the full numbers.
     *
     * @param array<string,mixed>|string $request
     * @return \Generator<int,array{0:array<string,mixed>,1:string}>
     */
    public function streamGenerator(string $method, $request): \Generator
    {
        $this->assertOpen();

        $json = \is_string($request) ? $request : self::encode($request);

        // Everything the fiber's closure touches ($this->ffi, $this->handle)
        // is read once here and captured, the same as any other use($this).
        $fiber = new \Fiber(function () use ($method, $json): array {
            $trampoline = function ($chunkJson, $userData) {
                $raw = \is_string($chunkJson) ? $chunkJson : \FFI::string($chunkJson);
                \Fiber::suspend($raw);

                return 0;
            };
            $err = $this->ffi->new('char*');
            $rc = $this->ffi->llmux_stream($this->handle, $method, $json, $trampoline, null, \FFI::addr($err));

            return [$rc, $rc === 0 ? null : $this->takeError($err)];
        });

        try {
            $raw = $fiber->start();
            while (!$fiber->isTerminated()) {
                $decoded = \json_decode($raw, true);
                yield [\is_array($decoded) ? $decoded : [], $raw];
                $raw = $fiber->resume();
            }
        } finally {
            if (!$fiber->isTerminated()) {
                // Abandoned mid-stream. A suspended Fiber cannot simply be
                // dropped — PHP raises FiberError out of a suspended Fiber's
                // own destructor — and the llmux_stream call parked inside
                // this one is still open, so cancel() (not close(), which
                // would try to wait on this very call) is what gets it to
                // return. Doing that HERE, in this finally, rather than
                // leaving it for whenever the fiber object itself would be
                // collected, is what makes the cancellation immediate instead
                // of merely eventual.
                $this->cancel();
                while (!$fiber->isTerminated()) {
                    $fiber->resume();
                }
            }
        }

        [$rc, $err] = $fiber->getReturn();
        if ($rc !== 0 && $err !== 'context canceled') {
            // A cancellation this method asked for, above, is not a failure
            // worth throwing over; any OTHER error (a real upstream failure)
            // still is.
            throw new LlmuxException("llmux_stream({$method}): {$err}");
        }
    }

    /**
     * Abort every call in flight on this handle, without closing it: the
     * handle stays open and usable, and the next call starts on a fresh
     * context.
     *
     * This is the only way to get a blocked call() or stream() to return
     * early from OUTSIDE the call itself — which matters here specifically
     * because PHP's common CLI/FPM builds have no threads, so "outside the
     * call" can only mean a Fiber that has stepped away from it (see
     * streamGenerator()) or a pcntl signal handler (see README.md — measured,
     * and more fragile than the Fiber path). Calling cancel() from INSIDE a
     * stream() callback also works and is simpler when that is all you need.
     *
     * Measured on this machine, against a fake upstream sleeping 100 ms per
     * chunk (sdks/fake-upstream.py), cancelling a ten-word stream after 3
     * delivered chunks: the blocked stream() call returns/throws with
     * "context canceled", the consumer sees exactly 3 chunks, and the
     * upstream's own count at GET /generated also reads 3 — against 12 for a
     * run left to finish. See README.md's Cancellation section for the full
     * numbers.
     *
     * Cancelling is per HANDLE, not per call: it aborts every call in flight
     * on this gateway, not just the one you meant to stop. Give a
     * cancellation scope its own gateway if it needs to be independent.
     *
     * Cancelling an unknown or idle handle is a no-op, so cleanup paths can
     * call it blindly, the same as close().
     */
    public function cancel(): void
    {
        if ($this->handle !== 0) {
            $this->ffi->llmux_cancel($this->handle);
        }
    }

    /**
     * Release the gateway. Idempotent, so cleanup paths can call it blindly.
     */
    public function close(): void
    {
        if ($this->handle !== 0) {
            $this->ffi->llmux_close($this->handle);
            $this->handle = 0;
        }
    }

    /**
     * A safety net, not the mechanism. PHP destroys objects at unpredictable
     * points and not at all on a fatal error; close() in a finally block is
     * what actually guarantees the handle goes away.
     */
    public function __destruct()
    {
        $this->close();
    }

    // ---------------------------------------------------------------- internals

    private function assertOpen(): void
    {
        if ($this->handle === 0) {
            throw new LlmuxException('this Llmux\Ffi handle is closed');
        }
    }

    /**
     * Read the message out of an err slot and free it. Every non-const char*
     * libllmux returns — results and error messages alike — is released with
     * llmux_free and with nothing else.
     *
     * @param \FFI\CData $err a `char*` slot
     */
    private function takeError($err): string
    {
        if (\FFI::isNull($err)) {
            return '(no message)';
        }
        $msg = \FFI::string($err);
        $this->ffi->llmux_free($err);

        return $msg;
    }

    private function looksRestricted(\FFI\Exception $e): bool
    {
        return \strpos($e->getMessage(), 'ffi.enable') !== false;
    }

    /** @param array<string,mixed> $value */
    private static function encode(array $value): string
    {
        $json = \json_encode($value);
        if ($json === false) {
            throw new LlmuxException('could not encode the request as JSON: ' . \json_last_error_msg());
        }

        return $json;
    }

    /**
     * Find libllmux:
     *   1. LLMUX_LIBRARY
     *   2. lib/ inside this package (where a platform package would put it)
     *   3. dist/ffi/<goos>_<goarch>/ in a checkout of the llmux repo
     *   4. the bare soname, letting the dynamic loader search its own paths
     */
    private static function resolveLibrary(): string
    {
        $env = \getenv('LLMUX_LIBRARY');
        if ($env !== false && $env !== '') {
            return $env;
        }

        $name = self::libraryName();
        $pkg = \dirname(__DIR__);  // .../sdks/php
        $repo = \dirname($pkg, 2); // .../sdks/php -> .../sdks -> repo root

        $candidates = [
            $pkg . '/lib/' . $name,
            $repo . '/dist/ffi/' . self::goos() . '_' . self::goarch() . '/' . $name,
        ];
        foreach ($candidates as $candidate) {
            if (\is_file($candidate)) {
                return $candidate;
            }
        }

        // Let the dynamic loader try. If it fails, the FFI\Exception naming the
        // soname is more useful than a guess at a path.
        return $name;
    }

    private static function libraryName(): string
    {
        switch (\PHP_OS_FAMILY) {
            case 'Windows':
                return 'llmux.dll';
            case 'Darwin':
                return 'libllmux.dylib';
            default:
                return 'libllmux.so';
        }
    }

    private static function goos(): string
    {
        switch (\PHP_OS_FAMILY) {
            case 'Windows':
                return 'windows';
            case 'Darwin':
                return 'darwin';
            case 'BSD':
                return 'freebsd';
            default:
                return 'linux';
        }
    }

    private static function goarch(): string
    {
        $machine = \strtolower(\php_uname('m'));
        if ($machine === 'arm64' || $machine === 'aarch64') {
            return 'arm64';
        }
        if ($machine === 'x86_64' || $machine === 'amd64') {
            return 'amd64';
        }

        return $machine;
    }
}
