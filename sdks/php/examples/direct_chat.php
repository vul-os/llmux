<?php

declare(strict_types=1);

/**
 * DIRECT (in-process) — llmux through the C ABI, no child process and no port.
 *
 *   php sdks/php/examples/direct_chat.php
 *
 * Environment:
 *   LLMUX_LIBRARY      path to libllmux.dylib/.so (default: see Ffi::resolveLibrary)
 *   LLMUX_CONFIG_JSON  an llmux config document, inline. Unset means built-in
 *                      defaults plus the environment (OPENAI_API_KEY, ...).
 *   LLMUX_MODEL        model to ask (default: openai/gpt-4o-mini)
 *
 * Before you copy this into a web application, read sdks/php/README.md. Under
 * php-fpm this is measurably the wrong shape: use sidecar_chat.php.
 */

require __DIR__ . '/../src/LlmuxException.php';
require __DIR__ . '/../src/Ffi.php';

use Llmux\Ffi;
use Llmux\LlmuxException;

$config = ($c = getenv('LLMUX_CONFIG_JSON')) !== false && $c !== '' ? $c : null;
$model = ($m = getenv('LLMUX_MODEL')) !== false && $m !== '' ? $m : 'openai/gpt-4o-mini';

try {
    $llmux = new Ffi($config);
} catch (LlmuxException $e) {
    fwrite(STDERR, "could not load libllmux: {$e->getMessage()}\n");
    exit(1);
}

// try/finally is the whole point: every path below — a thrown exception, an
// early return, a failed call — still closes the handle exactly once.
try {
    echo "library : {$llmux->libraryPath()}\n";
    echo "abi     : {$llmux->version()}\n";

    // 1. A call that touches no upstream: the routing table itself.
    $models = $llmux->call('models');
    $ids = array_map(static fn (array $m): string => $m['id'], $models['data']);
    printf("models  : %d (%s%s)\n", count($ids), implode(', ', array_slice($ids, 0, 3)),
        count($ids) > 3 ? ', …' : '');

    // 2. A unary chat completion.
    $answer = $llmux->call('chat', [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'Say hello in five words.']],
    ]);
    echo "chat    : ", trim($answer['choices'][0]['message']['content']), "\n";

    // 3. The same request, streamed. The callback runs on THIS thread,
    //    synchronously, before stream() returns — measured in ffi/ctest/smoke.c,
    //    not assumed — so there is nothing to synchronise and no GIL analogue
    //    to worry about.
    echo 'stream  : ';
    $chunks = $llmux->stream('chat', [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'Say hello in five words.']],
    ], static function (array $chunk): void {
        $delta = $chunk['choices'][0]['delta']['content'] ?? '';
        if ($delta !== '') {
            echo $delta;
            flush();
        }
    });
    echo "\n          ({$chunks} chunks)\n";

    // 4. Stopping early. Returning false from the callback stops the stream and
    //    is NOT an error — llmux returns 0 and writes no message.
    $seen = 0;
    $llmux->stream('chat', [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'Count to fifty.']],
    ], static function () use (&$seen): bool {
        return ++$seen < 3;   // false on the 3rd chunk
    });
    echo "early   : stopped after {$seen} chunks, no error\n";

    // 5. The error path. The message llmux writes to *err is freed with
    //    llmux_free inside callRaw() before the exception is thrown — an error
    //    is not a leak.
    try {
        $llmux->call('summon-a-demon', []);
        echo "error   : UNEXPECTED — an unknown method should fail\n";
    } catch (LlmuxException $e) {
        echo "error   : {$e->getMessage()}\n";
    }
} finally {
    $llmux->close();
}

echo "closed  : ok\n";
