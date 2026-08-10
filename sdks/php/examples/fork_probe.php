<?php

declare(strict_types=1);

/**
 * The fork probe — evidence, not a claim.
 *
 * libllmux puts the Go runtime in your process, and the Go runtime does not
 * survive fork() without exec(). php-fpm is a pre-forking server. This script
 * makes the collision reproducible on your own machine, in about 30 seconds:
 *
 *   php sdks/php/examples/fork_probe.php before chat   # expect: HUNG
 *   php sdks/php/examples/fork_probe.php after  chat   # expect: exited 0
 *   php sdks/php/examples/fork_probe.php before models # expect: HUNG, after answering
 *
 * The third line is the point, and it changed shape in 0.1.5 — for the better.
 *
 * `models` is answered from memory and needs nothing from the Go scheduler or
 * netpoller, so it still succeeds in a broken child: you will see
 * `child: models returned 1580 bytes` scroll past. Through 0.1.4 the child then
 * exited 0, and a smoke test built on `models` reported a healthy system. That
 * was the trap: the difference between a passing test and production was which
 * method you happened to call.
 *
 * Since 0.1.5 `llmux_close` drains in-flight work with a grace period, and a
 * fork-broken runtime cannot complete that drain — so the child now hangs on
 * the way out instead of exiting 0. The false green is gone: the answer is
 * still wrong-headed, but the process no longer pretends otherwise. `chat`
 * hangs as it always did, on the call rather than the close.
 *
 * That the fix for a different problem removed this one is luck, not design.
 * Do not rely on close() to catch a fork for you — use the `spawn` start
 * method, or the sidecar.
 *
 * Requires ext-pcntl (CLI only). Environment: LLMUX_LIBRARY, LLMUX_CONFIG_JSON.
 */

require __DIR__ . '/../src/LlmuxException.php';
require __DIR__ . '/../src/Ffi.php';

use Llmux\Ffi;
use Llmux\LlmuxException;

if (!function_exists('pcntl_fork')) {
    fwrite(STDERR, "ext-pcntl is required (it is CLI-only and not built into every PHP)\n");
    exit(2);
}

$when = $argv[1] ?? 'before';     // before | after   (relative to the fork)
$method = $argv[2] ?? 'chat';     // chat | models
$timeout = (float) ($argv[3] ?? 10.0);

if (!in_array($when, ['before', 'after'], true)) {
    fwrite(STDERR, "usage: fork_probe.php [before|after] [chat|models] [timeout_seconds]\n");
    exit(2);
}

$config = ($c = getenv('LLMUX_CONFIG_JSON')) !== false && $c !== '' ? $c : null;
$model = ($m = getenv('LLMUX_MODEL')) !== false && $m !== '' ? $m : 'openai/gpt-4o-mini';
$request = $method === 'chat'
    ? ['model' => $model, 'messages' => [['role' => 'user', 'content' => 'hi']]]
    : null;

$llmux = null;
if ($when === 'before') {
    // The php-fpm shape: the library is loaded in the parent, and every worker
    // is a fork() of it. opcache.preload does exactly this.
    $llmux = new Ffi($config);
}

$pid = pcntl_fork();
if ($pid === -1) {
    fwrite(STDERR, "fork failed\n");
    exit(2);
}

if ($pid === 0) {
    // ---- child ("worker") --------------------------------------------------
    try {
        if ($when === 'after') {
            // The safe shape: each worker dlopen()s the library itself, so the
            // Go runtime it uses was started after the fork.
            $llmux = new Ffi($config);
        }
        try {
            $bytes = strlen($llmux->callRaw($method, $request));
            fwrite(STDERR, "  child: {$method} returned {$bytes} bytes\n");
        } finally {
            $llmux->close();
        }
        exit(0);
    } catch (LlmuxException $e) {
        fwrite(STDERR, "  child: ERROR {$e->getMessage()}\n");
        exit(1);
    }
}

// ---- parent ("master") -----------------------------------------------------
try {
    $deadline = microtime(true) + $timeout;
    while (microtime(true) < $deadline) {
        $status = 0;
        if (pcntl_waitpid($pid, $status, WNOHANG) === $pid) {
            $verdict = pcntl_wifexited($status)
                ? 'exited ' . pcntl_wexitstatus($status)
                : 'killed by signal ' . pcntl_wtermsig($status);
            printf("load=%-6s method=%-6s -> child %s\n", $when, $method, $verdict);
            exit(0);
        }
        usleep(50_000);
    }
    posix_kill($pid, SIGKILL);
    pcntl_waitpid($pid, $status);
    printf("load=%-6s method=%-6s -> child HUNG (SIGKILLed after %.0fs)\n", $when, $method, $timeout);
} finally {
    if ($llmux !== null) {
        $llmux->close();
    }
}
