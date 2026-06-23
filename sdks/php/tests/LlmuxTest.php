<?php

declare(strict_types=1);

namespace Llmux\Tests;

use Llmux\Llmux;
use Llmux\LlmuxException;
use PHPUnit\Framework\TestCase;

/**
 * Tests for the llmux PHP sidecar launcher.
 *
 * Run from sdks/php:  composer install && vendor/bin/phpunit
 *
 * Covers binary resolution, URL formatting, health-poll readiness/timeout,
 * singleton/lazy start, cleanup, and an integration test gated on the real
 * binary. Most tests drive a fake fixture (a tiny python HTTP server honoring
 * LLMUX_ADDR) via the LLMUX_BINARY override, so no real gateway is needed.
 */
final class LlmuxTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/fixtures/fake_llmux.py';

    protected function setUp(): void
    {
        Llmux::stop();
    }

    protected function tearDown(): void
    {
        Llmux::stop();
        putenv('LLMUX_BINARY');
    }

    private static function python(): ?string
    {
        foreach (['python3', 'python'] as $c) {
            $path = trim((string) shell_exec('command -v ' . escapeshellarg($c) . ' 2>/dev/null'));
            if ($path !== '') {
                return $path;
            }
        }

        return null;
    }

    /** Write an executable shell wrapper that runs the python fake fixture. */
    private static function makeFake(array $extraEnv = []): string
    {
        $py = self::python();
        self::assertNotNull($py, 'python required for the fake fixture');
        $dir = sys_get_temp_dir() . '/llmux-php-fake-' . bin2hex(random_bytes(4));
        mkdir($dir, 0o755, true);
        $wrapper = $dir . '/llmux';
        $exports = '';
        foreach ($extraEnv as $k => $v) {
            $exports .= "export {$k}=\"{$v}\"\n";
        }
        file_put_contents($wrapper, "#!/bin/sh\n{$exports}exec \"{$py}\" \"" . self::FIXTURE . "\"\n");
        chmod($wrapper, 0o755);

        return $wrapper;
    }

    private static function portOpen(int $port): bool
    {
        $sock = @fsockopen('127.0.0.1', $port, $errno, $errstr, 0.3);
        if ($sock) {
            fclose($sock);

            return true;
        }

        return false;
    }

    private static function waitPortClosed(int $port, float $timeout): bool
    {
        $deadline = microtime(true) + $timeout;
        while (self::portOpen($port) && microtime(true) < $deadline) {
            usleep(50_000);
        }

        return !self::portOpen($port);
    }

    private static function healthStatus(string $base): int
    {
        $ctx = stream_context_create(['http' => ['timeout' => 1, 'ignore_errors' => true]]);
        @file_get_contents($base . '/health', false, $ctx);
        foreach ($http_response_header ?? [] as $h) {
            if (preg_match('#^HTTP/\S+\s+(\d+)#', $h, $m)) {
                return (int) $m[1];
            }
        }

        return 0;
    }

    private static function portOf(string $base): int
    {
        return (int) substr($base, strrpos($base, ':') + 1);
    }

    // --- binary resolution -------------------------------------------------

    public function testEnvOverrideWins(): void
    {
        $fake = self::makeFake();
        putenv("LLMUX_BINARY={$fake}");
        // Indirect proof: starting spawns the fake and becomes healthy, which is
        // only possible if the override path was used.
        $base = Llmux::start();
        self::assertSame(200, self::healthStatus($base));
    }

    public function testClearErrorWhenMissing(): void
    {
        // Force no override, an empty PATH, and (assuming) no bundled bin.
        putenv('LLMUX_BINARY');
        $bundled = \dirname(__DIR__) . '/bin/llmux';
        if (is_file($bundled)) {
            self::markTestSkipped('bundled bin/llmux present; cannot assert not-found path');
        }
        $savedPath = getenv('PATH');
        putenv('PATH=');
        try {
            $this->expectException(LlmuxException::class);
            $this->expectExceptionMessage('llmux binary not found');
            Llmux::start();
        } finally {
            putenv('PATH=' . $savedPath);
        }
    }

    // --- URL formatting ----------------------------------------------------

    public function testOpenAiBaseUrlAppendsV1(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake());
        $base = Llmux::baseUrl();
        self::assertMatchesRegularExpression('#^http://127\.0\.0\.1:\d+$#', $base);
        self::assertSame($base . '/v1', Llmux::openaiBaseUrl());
        self::assertStringEndsWith('/v1', Llmux::openaiBaseUrl());
    }

    // --- health-poll logic -------------------------------------------------

    public function testBecomesReadyOn200(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake());
        $base = Llmux::start(['timeout' => 10.0]);
        self::assertSame(200, self::healthStatus($base));
    }

    public function testTimesOutWhenNever200(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake(['FAKE_HEALTH_STATUS' => '503']));
        $this->expectException(LlmuxException::class);
        $this->expectExceptionMessage('did not become healthy');
        Llmux::start(['timeout' => 0.6]);
    }

    public function testTimesOutWhenUnreachable(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake(['FAKE_NEVER_LISTEN' => '1']));
        $this->expectException(LlmuxException::class);
        Llmux::start(['timeout' => 0.6]);
    }

    // --- singleton / lazy start -------------------------------------------

    public function testStartTwiceSameBaseNoRespawn(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake());
        $b1 = Llmux::start();
        $b2 = Llmux::start();
        $b3 = Llmux::baseUrl();
        self::assertSame($b1, $b2);
        self::assertSame($b1, $b3);
    }

    // --- cleanup -----------------------------------------------------------

    public function testStopKillsChildAndFreesPort(): void
    {
        putenv('LLMUX_BINARY=' . self::makeFake());
        $base = Llmux::start();
        $port = self::portOf($base);
        self::assertTrue(self::portOpen($port), 'port should be open while running');
        Llmux::stop();
        self::assertTrue(self::waitPortClosed($port, 3.0), 'port should be freed after stop');
    }

    // --- integration (real binary) ----------------------------------------

    public function testIntegrationRealBinary(): void
    {
        $real = getenv('LLMUX_BINARY_REAL') ?: null;
        $bundled = \dirname(__DIR__) . '/bin/llmux';
        $bin = $real ?: (is_file($bundled) ? $bundled : null);
        if ($bin === null) {
            self::markTestSkipped('real llmux binary not available');
        }
        putenv("LLMUX_BINARY={$bin}");
        $base = Llmux::start(['timeout' => 15.0]);
        self::assertMatchesRegularExpression('#^http://127\.0\.0\.1:\d+$#', $base);
        self::assertSame(200, self::healthStatus($base));
        self::assertStringEndsWith('/v1', Llmux::openaiBaseUrl());
    }
}
