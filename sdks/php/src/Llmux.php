<?php

declare(strict_types=1);

namespace Llmux;

/**
 * llmux — the LLM multiplexer, embedded locally for PHP.
 *
 *   use Llmux\Llmux;
 *
 *   $base = Llmux::baseUrl();        // http://127.0.0.1:<port>
 *   $v1   = Llmux::openaiBaseUrl();  // http://127.0.0.1:<port>/v1
 *
 *   // Or a configured openai-php client (optional openai-php/client):
 *   $client = Llmux::openai();
 *   $r = $client->chat()->create([
 *       'model'    => 'anthropic/claude-3-5-sonnet',
 *       'messages' => [['role' => 'user', 'content' => 'hi']],
 *   ]);
 *
 * No server to run: the gateway starts as a local child process and your
 * existing OpenAI client points at it. Provider keys come from env vars
 * (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, ...).
 */
final class Llmux
{
    public const VERSION = '0.1.0';

    /** @var resource|null */
    private static $proc = null;

    /** @var string|null */
    private static $base = null;

    /** @var bool */
    private static $shutdownRegistered = false;

    /**
     * Start the sidecar (idempotent). Returns the base URL (http://host:port).
     *
     * Provider API keys are inherited from the environment (OPENAI_API_KEY,
     * etc.), so the gateway auto-detects providers like the standalone binary.
     *
     * @param array{port?:int,config?:string,env?:array<string,string>,timeout?:float} $opts
     */
    public static function start(array $opts = []): string
    {
        if (self::running()) {
            return self::$base;
        }

        $port = $opts['port'] ?? self::freePort();
        $addr = "127.0.0.1:{$port}";

        $env = self::inheritedEnv();
        $env['LLMUX_ADDR'] = $addr;
        if (isset($opts['config'])) {
            $env['LLMUX_CONFIG'] = $opts['config'];
        }
        if (isset($opts['env'])) {
            $env = array_merge($env, $opts['env']);
        }

        $descriptors = [
            0 => STDIN,
            1 => STDOUT,
            2 => STDERR,
        ];

        $proc = proc_open([self::binaryPath()], $descriptors, $pipes, null, $env);
        if (!\is_resource($proc)) {
            throw new LlmuxException('failed to spawn llmux binary');
        }
        self::$proc = $proc;
        self::$base = "http://{$addr}";

        try {
            self::waitHealthy(self::$base, (float) ($opts['timeout'] ?? 10.0));
        } catch (\Throwable $e) {
            self::stop();
            throw $e;
        }

        if (!self::$shutdownRegistered) {
            register_shutdown_function([self::class, 'stop']);
            self::$shutdownRegistered = true;
        }

        return self::$base;
    }

    /** The running base URL (http://host:port), starting the sidecar if needed. */
    public static function baseUrl(): string
    {
        if (self::running()) {
            return self::$base;
        }

        return self::start();
    }

    /** The OpenAI-style base URL (…/v1) for SDK baseUri arguments. */
    public static function openaiBaseUrl(): string
    {
        return self::baseUrl() . '/v1';
    }

    /** Stop the sidecar if running. */
    public static function stop(): void
    {
        if (\is_resource(self::$proc)) {
            $status = proc_get_status(self::$proc);
            if ($status['running']) {
                proc_terminate(self::$proc, \defined('SIGTERM') ? SIGTERM : 15);
                $deadline = microtime(true) + 5.0;
                while (microtime(true) < $deadline) {
                    $status = proc_get_status(self::$proc);
                    if (!$status['running']) {
                        break;
                    }
                    usleep(50_000);
                }
                $status = proc_get_status(self::$proc);
                if ($status['running']) {
                    proc_terminate(self::$proc, \defined('SIGKILL') ? SIGKILL : 9);
                }
            }
            proc_close(self::$proc);
        }
        self::$proc = null;
    }

    /**
     * Return an `openai-php/client` pointed at the local gateway.
     * Requires the optional `openai-php/client` package.
     *
     * @param string $apiKey
     */
    public static function openai(string $apiKey = 'llmux-local')
    {
        if (!\class_exists('\\OpenAI')) {
            throw new LlmuxException(
                'openai-php/client is not installed. Run: composer require openai-php/client'
            );
        }

        return \OpenAI::factory()
            ->withApiKey($apiKey)
            ->withBaseUri(self::baseUrl() . '/v1')
            ->make();
    }

    private static function running(): bool
    {
        if (!\is_resource(self::$proc)) {
            return false;
        }
        $status = proc_get_status(self::$proc);

        return (bool) $status['running'];
    }

    private static function binaryPath(): string
    {
        // 1) explicit override
        $env = getenv('LLMUX_BINARY');
        if ($env !== false && $env !== '') {
            return $env;
        }

        // 2) binary bundled in the package
        $name = self::isWindows() ? 'llmux.exe' : 'llmux';
        $bundled = \dirname(__DIR__) . DIRECTORY_SEPARATOR . 'bin' . DIRECTORY_SEPARATOR . $name;
        if (is_file($bundled)) {
            return $bundled;
        }

        // 3) on PATH
        $found = self::which('llmux');
        if ($found !== null) {
            return $found;
        }

        throw new LlmuxException(
            'llmux binary not found. Set LLMUX_BINARY, install a platform ' .
            'package, or build it: `go build -o sdks/php/bin/llmux ./cmd/llmux`'
        );
    }

    private static function which(string $cmd): ?string
    {
        $exts = self::isWindows() ? explode(';', (string) getenv('PATHEXT')) : [''];
        foreach (explode(PATH_SEPARATOR, (string) getenv('PATH')) as $dir) {
            foreach ($exts as $ext) {
                $candidate = $dir . DIRECTORY_SEPARATOR . $cmd . $ext;
                if (is_file($candidate) && is_executable($candidate)) {
                    return $candidate;
                }
            }
        }

        return null;
    }

    private static function isWindows(): bool
    {
        return \PHP_OS_FAMILY === 'Windows';
    }

    /** @return array<string,string> */
    private static function inheritedEnv(): array
    {
        $env = [];
        foreach ($_ENV as $k => $v) {
            $env[(string) $k] = (string) $v;
        }
        // $_ENV may be empty depending on variables_order; fall back to getenv.
        if ($env === []) {
            foreach (\array_keys(getenv()) as $k) {
                $env[(string) $k] = (string) getenv((string) $k);
            }
        }

        return $env;
    }

    private static function freePort(): int
    {
        $sock = @stream_socket_server('tcp://127.0.0.1:0', $errno, $errstr);
        if ($sock === false) {
            throw new LlmuxException("could not allocate a free port: {$errstr}");
        }
        $name = stream_socket_get_name($sock, false);
        fclose($sock);
        $port = (int) substr((string) $name, (int) strrpos((string) $name, ':') + 1);

        return $port;
    }

    private static function waitHealthy(string $base, float $timeout): void
    {
        $deadline = microtime(true) + $timeout;
        $last = '';
        $ctx = stream_context_create(['http' => ['timeout' => 1, 'ignore_errors' => true]]);
        while (microtime(true) < $deadline) {
            $body = @file_get_contents($base . '/health', false, $ctx);
            if ($body !== false && isset($http_response_header)) {
                $status = self::parseStatus($http_response_header);
                if ($status === 200) {
                    return;
                }
                $last = "status {$status}";
            } else {
                $last = 'connection refused';
            }
            usleep(50_000);
        }
        throw new LlmuxException("llmux did not become healthy within {$timeout}s: {$last}");
    }

    /** @param array<int,string> $headers */
    private static function parseStatus(array $headers): int
    {
        foreach ($headers as $h) {
            if (preg_match('#^HTTP/\S+\s+(\d+)#', $h, $m)) {
                return (int) $m[1];
            }
        }

        return 0;
    }
}
