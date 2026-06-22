package to.llmux;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.Map;

/**
 * llmux — the LLM multiplexer, embedded locally for Java.
 *
 * <p>The local wedge: instead of running a server yourself, this starts the
 * gateway as a child process on {@code 127.0.0.1} and hands you a base URL.
 * Point any OpenAI-compatible client at it.
 *
 * <pre>{@code
 *   String base = Llmux.baseUrl();        // http://127.0.0.1:<port>
 *   String v1 = Llmux.openaiBaseUrl();    // http://127.0.0.1:<port>/v1
 * }</pre>
 *
 * <p>Provider keys are inherited from the environment (OPENAI_API_KEY,
 * ANTHROPIC_API_KEY, GEMINI_API_KEY, …).
 */
public final class Llmux {

    public static final String VERSION = "0.1.0";

    private static final Object LOCK = new Object();
    private static Process proc;
    private static String base;
    private static boolean shutdownHookRegistered;

    private Llmux() {}

    /** Options for {@link #start(Options)}. */
    public static final class Options {
        /** Fixed port; defaults to an ephemeral free port. */
        public Integer port;
        /** Path to a JSON config file. */
        public String config;
        /** Extra environment variables for the child process. */
        public Map<String, String> env;
        /** Health-check timeout (default 10s). */
        public Duration timeout;
    }

    /** Start the sidecar (idempotent). Returns the base URL (http://host:port). */
    public static String start(Options opts) {
        synchronized (LOCK) {
            if (proc != null && proc.isAlive()) {
                return base;
            }
            if (opts == null) {
                opts = new Options();
            }
            int port = opts.port != null ? opts.port : freePort();
            String addr = "127.0.0.1:" + port;

            ProcessBuilder pb = new ProcessBuilder(binaryPath());
            pb.inheritIO();
            Map<String, String> environment = pb.environment();
            environment.put("LLMUX_ADDR", addr);
            if (opts.config != null) {
                environment.put("LLMUX_CONFIG", opts.config);
            }
            if (opts.env != null) {
                environment.putAll(opts.env);
            }

            try {
                proc = pb.start();
            } catch (IOException e) {
                throw new LlmuxException("failed to spawn llmux binary", e);
            }
            base = "http://" + addr;

            Duration timeout = opts.timeout != null ? opts.timeout : Duration.ofSeconds(10);
            try {
                waitHealthy(base, timeout);
            } catch (RuntimeException e) {
                stop();
                throw e;
            }

            if (!shutdownHookRegistered) {
                Runtime.getRuntime().addShutdownHook(new Thread(Llmux::stop));
                shutdownHookRegistered = true;
            }
            return base;
        }
    }

    /** The running base URL (http://host:port), starting the sidecar if needed. */
    public static String baseUrl() {
        synchronized (LOCK) {
            if (proc != null && proc.isAlive()) {
                return base;
            }
        }
        return start(null);
    }

    /** The OpenAI-style base URL (…/v1). */
    public static String openaiBaseUrl() {
        return baseUrl() + "/v1";
    }

    /** Stop the sidecar if running. */
    public static void stop() {
        synchronized (LOCK) {
            if (proc != null && proc.isAlive()) {
                proc.destroy();
                try {
                    if (!proc.waitFor(5, java.util.concurrent.TimeUnit.SECONDS)) {
                        proc.destroyForcibly();
                    }
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    proc.destroyForcibly();
                }
            }
            proc = null;
        }
    }

    private static String binaryPath() {
        // 1) explicit override
        String env = System.getenv("LLMUX_BINARY");
        if (env != null && !env.isEmpty()) {
            return env;
        }
        // 2) binary bundled next to this class's resources
        boolean windows = System.getProperty("os.name", "").toLowerCase().contains("win");
        String name = windows ? "llmux.exe" : "llmux";
        Path bundled = bundledDir().resolve("bin").resolve(name);
        if (Files.isRegularFile(bundled)) {
            return bundled.toString();
        }
        // 3) on PATH
        String found = which("llmux", windows);
        if (found != null) {
            return found;
        }
        throw new LlmuxException(
                "llmux binary not found. Set LLMUX_BINARY, or build it: "
                        + "`go build -o sdks/java/bin/llmux ./cmd/llmux`");
    }

    /** Resolve the package's bin/ dir from the LLMUX_HOME env or the module dir. */
    private static Path bundledDir() {
        String home = System.getenv("LLMUX_HOME");
        if (home != null && !home.isEmpty()) {
            return Paths.get(home);
        }
        // Locate the directory containing the running classes/jar, then look for
        // a sibling bin/ (works when bin/ ships alongside the jar).
        try {
            Path self = Paths.get(
                    Llmux.class.getProtectionDomain().getCodeSource().getLocation().toURI());
            Path dir = Files.isDirectory(self) ? self : self.getParent();
            return dir != null ? dir : Paths.get(".");
        } catch (Exception e) {
            return Paths.get(".");
        }
    }

    private static String which(String cmd, boolean windows) {
        String path = System.getenv("PATH");
        if (path == null) {
            return null;
        }
        for (String dir : path.split(java.io.File.pathSeparator)) {
            Path candidate = Paths.get(dir, cmd);
            if (Files.isRegularFile(candidate) && Files.isExecutable(candidate)) {
                return candidate.toString();
            }
            if (windows) {
                Path exe = Paths.get(dir, cmd + ".exe");
                if (Files.isRegularFile(exe)) {
                    return exe.toString();
                }
            }
        }
        return null;
    }

    private static int freePort() {
        try (ServerSocket s = new ServerSocket()) {
            s.bind(new InetSocketAddress("127.0.0.1", 0));
            return s.getLocalPort();
        } catch (IOException e) {
            throw new LlmuxException("could not allocate a free port", e);
        }
    }

    private static void waitHealthy(String base, Duration timeout) {
        HttpClient client = HttpClient.newBuilder()
                .connectTimeout(Duration.ofSeconds(1))
                .build();
        HttpRequest req = HttpRequest.newBuilder(URI.create(base + "/health"))
                .timeout(Duration.ofSeconds(1))
                .GET()
                .build();
        long deadline = System.nanoTime() + timeout.toNanos();
        String last = "connection refused";
        while (System.nanoTime() < deadline) {
            try {
                HttpResponse<Void> res = client.send(req, HttpResponse.BodyHandlers.discarding());
                if (res.statusCode() == 200) {
                    return;
                }
                last = "status " + res.statusCode();
            } catch (Exception e) {
                last = e.getMessage();
            }
            try {
                Thread.sleep(50);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            }
        }
        throw new LlmuxException("llmux did not become healthy within " + timeout + ": " + last);
    }
}
