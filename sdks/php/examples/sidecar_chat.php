<?php

declare(strict_types=1);

/**
 * SIDECAR (out-of-process) — the SDK spawns `llmux serve` on a loopback port,
 * waits for it to be healthy, and shuts it down at exit. You never run a
 * server by hand.
 *
 *   php sdks/php/examples/sidecar_chat.php
 *
 * Environment:
 *   LLMUX_BINARY  path to the llmux binary (default: bundled bin/llmux, then PATH)
 *   LLMUX_CONFIG  path to an llmux.json (optional)
 *   LLMUX_MODEL   model to ask (default: openai/gpt-4o-mini)
 *
 * This is the recommended shape for PHP. sdks/php/README.md explains why, with
 * the php-fpm measurements behind the claim.
 */

require __DIR__ . '/../src/LlmuxException.php';
require __DIR__ . '/../src/Llmux.php';

use Llmux\Llmux;
use Llmux\LlmuxException;

$model = ($m = getenv('LLMUX_MODEL')) !== false && $m !== '' ? $m : 'openai/gpt-4o-mini';

/**
 * A POST to the sidecar. cURL is used rather than file_get_contents so the
 * streaming case below can consume the SSE body as it arrives; without that,
 * "streaming" would just be a buffered response replayed as fake chunks.
 *
 * @param array<string,mixed> $body
 * @param callable(string):void|null $onData
 * @return array{0:int,1:string} [status, body]
 */
function post(string $url, array $body, ?callable $onData = null): array
{
    $ch = curl_init($url);
    $collected = '';
    curl_setopt_array($ch, [
        CURLOPT_POST => true,
        CURLOPT_POSTFIELDS => json_encode($body),
        CURLOPT_HTTPHEADER => ['Content-Type: application/json', 'Authorization: Bearer llmux-local'],
        CURLOPT_TIMEOUT => 120,
    ]);
    if ($onData !== null) {
        curl_setopt($ch, CURLOPT_WRITEFUNCTION, static function ($ch, string $data) use ($onData): int {
            $onData($data);

            return strlen($data);
        });
    } else {
        curl_setopt($ch, CURLOPT_RETURNTRANSFER, true);
    }

    try {
        $out = curl_exec($ch);
        if ($out === false) {
            throw new LlmuxException('curl: ' . curl_error($ch));
        }
        $status = (int) curl_getinfo($ch, CURLINFO_RESPONSE_CODE);

        return [$status, is_string($out) ? $out : $collected];
    } finally {
        // Since PHP 8.0 a cURL handle is an object the engine frees; curl_close()
        // is a no-op and is deprecated in 8.5. Dropping the last reference here
        // is what actually closes the connection, on the throw path too.
        unset($ch);
    }
}

try {
    // Starts the child process, waits for GET /health, and registers a shutdown
    // hook that terminates it. Idempotent — call it from anywhere.
    $base = Llmux::start();
    echo "sidecar : {$base}\n";
} catch (LlmuxException $e) {
    fwrite(STDERR, "could not start the sidecar: {$e->getMessage()}\n");
    exit(1);
}

// try/finally, so a failure below still stops the child rather than leaving an
// orphaned server on a port.
try {
    // 1. The routing table.
    $models = json_decode((string) file_get_contents(Llmux::openaiBaseUrl() . '/models'), true);
    $ids = array_map(static fn (array $m): string => $m['id'], $models['data']);
    printf("models  : %d (%s%s)\n", count($ids), implode(', ', array_slice($ids, 0, 3)),
        count($ids) > 3 ? ', …' : '');

    // 2. A unary chat completion — the identical JSON the C ABI takes.
    [$status, $body] = post(Llmux::openaiBaseUrl() . '/chat/completions', [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'Say hello in five words.']],
    ]);
    if ($status !== 200) {
        throw new LlmuxException("chat: HTTP {$status}: " . substr($body, 0, 200));
    }
    $answer = json_decode($body, true);
    echo 'chat    : ', trim($answer['choices'][0]['message']['content']), "\n";

    // 3. Streaming, over SSE. `data: {...}` frames, terminated by `data: [DONE]`
    //    — the same chunk objects llmux_stream hands a callback.
    echo 'stream  : ';
    $chunks = 0;
    $buffer = '';
    post(Llmux::openaiBaseUrl() . '/chat/completions', [
        'model' => $model,
        'messages' => [['role' => 'user', 'content' => 'Say hello in five words.']],
        'stream' => true,
    ], static function (string $data) use (&$chunks, &$buffer): void {
        $buffer .= $data;
        while (($nl = strpos($buffer, "\n")) !== false) {
            $line = rtrim(substr($buffer, 0, $nl), "\r");
            $buffer = substr($buffer, $nl + 1);
            if (strncmp($line, 'data: ', 6) !== 0) {
                continue;
            }
            $payload = substr($line, 6);
            if ($payload === '[DONE]') {
                return;
            }
            $chunk = json_decode($payload, true);
            $chunks++;
            $delta = $chunk['choices'][0]['delta']['content'] ?? '';
            if ($delta !== '') {
                echo $delta;
                flush();
            }
        }
    });
    echo "\n          ({$chunks} chunks)\n";

    // 4. The error path. Over HTTP an error is a status code and a JSON body,
    //    where the C ABI gives you a plain string in *err — the one place the
    //    two modes genuinely differ.
    [$status, $body] = post(Llmux::openaiBaseUrl() . '/chat/completions', [
        'model' => 'no-such-model-anywhere',
        'messages' => [['role' => 'user', 'content' => 'hi']],
    ]);
    echo "error   : HTTP {$status} ", trim(substr($body, 0, 160)), "\n";
} finally {
    // The shutdown hook would do this too; doing it explicitly means the port
    // is free the moment we are done with it.
    Llmux::stop();
}

echo "stopped : ok\n";
