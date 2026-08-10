<?php

declare(strict_types=1);

namespace Llmux\Tests;

use Llmux\Ffi;
use Llmux\LlmuxException;
use PHPUnit\Framework\TestCase;

/**
 * Tests for llmux_cancel through \Llmux\Ffi (direct/in-process mode).
 *
 * Run from sdks/php:  composer install && vendor/bin/phpunit
 *
 * Gated on two things neither of which is assumed present: a built libllmux
 * (LLMUX_LIBRARY, or dist/ffi/<goos>_<goarch>/ in a checkout) and python3 (to
 * run sdks/fake-upstream.py, the only upstream that can answer "how many
 * chunks did you actually generate?"). Skips cleanly when either is missing
 * rather than failing the suite.
 *
 * This is the regression test for the bug llmux_cancel exists to fix: a
 * consumer that stops reading after N chunks must not leave the upstream
 * generating (and metering) the rest in the background. See
 * sdks/php/README.md's Cancellation section for the numbers this reproduces
 * on this machine.
 */
final class FfiCancelTest extends TestCase
{
    private const FAKE_UPSTREAM = __DIR__ . '/../../fake-upstream.py';

    /** @var resource|null */
    private $upstream = null;

    protected function tearDown(): void
    {
        if (\is_resource($this->upstream)) {
            \proc_terminate($this->upstream);
            \proc_close($this->upstream);
        }
        $this->upstream = null;
    }

    private static function python(): ?string
    {
        foreach (['python3', 'python'] as $c) {
            $path = \trim((string) \shell_exec('command -v ' . \escapeshellarg($c) . ' 2>/dev/null'));
            if ($path !== '') {
                return $path;
            }
        }

        return null;
    }

    private static function library(): ?string
    {
        $env = \getenv('LLMUX_LIBRARY');
        if ($env !== false && $env !== '' && \is_file($env)) {
            return $env;
        }
        $repoRoot = \dirname(__DIR__, 3);
        $name = \PHP_OS_FAMILY === 'Darwin' ? 'libllmux.dylib' : (\PHP_OS_FAMILY === 'Windows' ? 'llmux.dll' : 'libllmux.so');
        $arch = \in_array(\strtolower(\php_uname('m')), ['arm64', 'aarch64'], true) ? 'arm64' : 'amd64';
        $goos = \PHP_OS_FAMILY === 'Darwin' ? 'darwin' : (\PHP_OS_FAMILY === 'Windows' ? 'windows' : 'linux');
        $candidate = "{$repoRoot}/dist/ffi/{$goos}_{$arch}/{$name}";

        return \is_file($candidate) ? $candidate : null;
    }

    /**
     * Spawn sdks/fake-upstream.py and return [url, configJson]. Stores the
     * process in $this->upstream so tearDown() can kill it.
     *
     * @return array{0:string,1:string}
     */
    private function startUpstream(int $chunkDelayMs, string $text): array
    {
        $python = self::python();
        if ($python === null) {
            self::markTestSkipped('python3 required for sdks/fake-upstream.py');
        }

        $descriptors = [0 => ['pipe', 'r'], 1 => ['pipe', 'w'], 2 => ['pipe', 'w']];
        $process = \proc_open([
            $python, self::FAKE_UPSTREAM,
            '--chunk-delay-ms', (string) $chunkDelayMs,
            '--text', $text,
        ], $descriptors, $pipes);
        self::assertIsResource($process, 'failed to spawn sdks/fake-upstream.py');
        \fclose($pipes[0]);
        $this->upstream = $process;

        $urlLine = \trim((string) \fgets($pipes[1]));
        $configLine = \trim((string) \fgets($pipes[1]));
        \fclose($pipes[1]);
        \fclose($pipes[2]);

        $url = \explode(' ', $urlLine, 2)[1] ?? null;
        $config = \explode(' ', $configLine, 2)[1] ?? null;
        self::assertNotNull($url, 'fake upstream did not print a URL line');
        self::assertNotNull($config, 'fake upstream did not print a CONFIG line');

        return [$url, $config];
    }

    private static function generatedCount(string $url): int
    {
        $body = \file_get_contents("{$url}/generated");
        self::assertIsString($body);
        $decoded = \json_decode($body, true);

        return (int) $decoded['generated'];
    }

    public function testStreamGeneratorBreakReachesLlmuxCancel(): void
    {
        $library = self::library();
        if ($library === null) {
            self::markTestSkipped('no libllmux available (set LLMUX_LIBRARY or run scripts/build-ffi.sh)');
        }

        [$url, $config] = $this->startUpstream(20, 'one two three four five six seven eight nine ten');

        $llmux = new Ffi($config, $library);
        try {
            $seen = 0;
            foreach ($llmux->streamGenerator('chat', [
                'model' => 'demo',
                'messages' => [['role' => 'user', 'content' => 'hi']],
            ]) as [$chunk, $raw]) {
                $seen++;
                if ($seen >= 3) {
                    break;
                }
            }
            self::assertSame(3, $seen, 'the consumer should see exactly the chunks generated before it broke');

            // Give the cancelled connection a moment to be observed upstream.
            \usleep(200_000);
            $generated = self::generatedCount($url);
            self::assertSame(
                3,
                $generated,
                "llmux_cancel should have stopped the upstream at 3 chunks, not let it run to {$generated}"
            );

            // The handle must survive: llmux_cancel aborts the call, not the gateway.
            self::assertArrayHasKey('data', $llmux->call('models'));
        } finally {
            $llmux->close();
        }
    }

    public function testCancelFromInsideCallbackStopsAStream(): void
    {
        $library = self::library();
        if ($library === null) {
            self::markTestSkipped('no libllmux available (set LLMUX_LIBRARY or run scripts/build-ffi.sh)');
        }

        [, $config] = $this->startUpstream(20, 'one two three four five six seven eight nine ten');

        $llmux = new Ffi($config, $library);
        try {
            $seen = 0;
            $threw = null;
            try {
                $llmux->stream('chat', [
                    'model' => 'demo',
                    'messages' => [['role' => 'user', 'content' => 'hi']],
                ], function () use (&$seen, $llmux): void {
                    $seen++;
                    if ($seen >= 3) {
                        $llmux->cancel();
                    }
                });
            } catch (LlmuxException $e) {
                $threw = $e;
            }

            self::assertSame(3, $seen);
            self::assertNotNull($threw, 'a stream cancelled mid-flight should throw, not return quietly');
            self::assertStringContainsString('context canceled', $threw->getMessage());
        } finally {
            $llmux->close();
        }
    }
}
