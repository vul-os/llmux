<?php

declare(strict_types=1);

/**
 * llmux_cancel, proven against an upstream that can tell you whether
 * cancelling actually stopped it.
 *
 *   php sdks/php/examples/cancel_demo.php
 *
 * direct_chat.php's "early stop" step (#4) shows the OTHER way to end a
 * stream — returning false from the callback — which is answered from
 * stream()'s own return value and never has to touch llmux_cancel at all
 * (measured: it already stops the provider too, not just delivery to your
 * callback). This script is about the harder question llmux_cancel exists to
 * answer: when a consumer stops reading after 3 of 10 chunks from OUTSIDE the
 * callback's own decision — a foreach that just breaks — did the PROVIDER
 * stop generating (and metering) the other 7, or did it run to completion in
 * the background while nobody was reading the rest? A generator whose
 * `foreach` loop simply stops being iterated cannot tell you that by itself.
 * sdks/fake-upstream.py can: it counts chunks it actually writes to the
 * socket and serves that count at GET /generated, so this script cancels,
 * then goes and asks the upstream what it did.
 *
 * This is a self-contained demo, deliberately not layered onto
 * direct_chat.php: that script's upstream is whatever LLMUX_CONFIG_JSON
 * points at (a real provider, or ffi/fakeupstream, neither of which can
 * answer "how many did you generate?"), and this measurement needs the
 * counting fake specifically. It spawns its own copy on a random port and
 * tears it down on exit.
 *
 * Requires PHP >= 8.1 (Fiber) — see Ffi::streamGenerator().
 */

require __DIR__ . '/../src/LlmuxException.php';
require __DIR__ . '/../src/Ffi.php';

use Llmux\Ffi;
use Llmux\LlmuxException;

$repoRoot = \dirname(__DIR__, 3);
$fakeUpstream = $repoRoot . '/sdks/fake-upstream.py';

$python = null;
foreach (['python3', 'python'] as $candidate) {
    $path = trim((string) shell_exec('command -v ' . escapeshellarg($candidate) . ' 2>/dev/null'));
    if ($path !== '') {
        $python = $path;
        break;
    }
}
if ($python === null) {
    fwrite(STDERR, "python3 is required to run sdks/fake-upstream.py\n");
    exit(2);
}

$descriptors = [0 => ['pipe', 'r'], 1 => ['pipe', 'w'], 2 => STDERR];
$process = proc_open([
    $python, $fakeUpstream,
    '--chunk-delay-ms', '100',
    '--text', 'one two three four five six seven eight nine ten',
], $descriptors, $pipes);
if (!\is_resource($process)) {
    fwrite(STDERR, "failed to spawn sdks/fake-upstream.py\n");
    exit(2);
}
fclose($pipes[0]);

$url = trim((string) fgets($pipes[1]));
$config = trim((string) fgets($pipes[1]));
[, $url] = \explode(' ', $url, 2) + [null, null];
[, $config] = \explode(' ', $config, 2) + [null, null];
if ($url === null || $config === null) {
    fwrite(STDERR, "fake upstream did not start\n");
    proc_terminate($process);
    exit(2);
}

try {
    $llmux = new Ffi($config);
    try {
        echo "upstream : {$url}\n";

        // The idiomatic construct: streamGenerator() returns a real
        // Generator, and breaking out of the foreach reaches llmux_cancel
        // through its `finally` — see the comment on Ffi::streamGenerator()
        // for why that `break` is safe here when a bare `break` inside
        // stream()'s own callback would not be.
        $seen = 0;
        echo 'consumer : ';
        foreach ($llmux->streamGenerator('chat', [
            'model' => 'demo',
            'messages' => [['role' => 'user', 'content' => 'hi']],
        ]) as [$chunk, $raw]) {
            $seen++;
            $delta = $chunk['choices'][0]['delta']['content'] ?? '';
            if ($delta !== '') {
                echo $delta;
                flush();
            }
            if ($seen >= 3) {
                break;
            }
        }
        echo "\n";
        echo "consumer : stopped after {$seen} chunks\n";

        // Query it AFTER the cancel demo above has finished, per the
        // harness's own contract — this is the number that proves (or
        // disproves) that cancelling actually stopped generation upstream,
        // not just delivery to us.
        $generated = \json_decode((string) file_get_contents("{$url}/generated"), true)['generated'] ?? null;
        printf(
            "upstream : generated %s chunks total (a run to completion would be 12 — " .
            "10 words plus a finish frame and a usage frame)\n",
            $generated ?? '?'
        );

        // The handle survives a cancel: llmux_cancel aborts the call, not the
        // gateway. A plain call on the same handle right afterward should
        // succeed.
        $llmux->call('models');
        echo "handle   : still usable after cancel\n";
    } finally {
        $llmux->close();
    }
} catch (LlmuxException $e) {
    fwrite(STDERR, "error: {$e->getMessage()}\n");
    proc_terminate($process);
    exit(1);
}

proc_terminate($process);
proc_close($process);
