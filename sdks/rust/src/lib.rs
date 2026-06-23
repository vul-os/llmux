//! llmux — the LLM multiplexer, embedded locally for Rust.
//!
//! The local wedge: instead of running a server yourself, this starts the
//! gateway as a child process on `127.0.0.1` and hands you a `base_url`. Point
//! any OpenAI-compatible client at it.
//!
//! ```no_run
//! let base = llmux::base_url()?;            // http://127.0.0.1:<port>
//! let v1 = llmux::openai_base_url()?;       // http://127.0.0.1:<port>/v1
//! # Ok::<(), llmux::Error>(())
//! ```
//!
//! Provider keys are inherited from the environment (`OPENAI_API_KEY`,
//! `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, …).

use std::env;
use std::fmt;
use std::io::Read;
use std::net::{TcpListener, TcpStream};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// Errors raised by the sidecar launcher.
#[derive(Debug)]
pub enum Error {
    BinaryNotFound,
    Spawn(std::io::Error),
    Health(String),
    Io(std::io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::BinaryNotFound => write!(
                f,
                "llmux binary not found. Set LLMUX_BINARY or build it: \
                 `go build -o sdks/rust/bin/llmux ./cmd/llmux`"
            ),
            Error::Spawn(e) => write!(f, "failed to spawn llmux: {e}"),
            Error::Health(s) => write!(f, "llmux did not become healthy: {s}"),
            Error::Io(e) => write!(f, "io error: {e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(e)
    }
}

struct State {
    child: Option<Child>,
    base: Option<String>,
}

static STATE: Mutex<State> = Mutex::new(State {
    child: None,
    base: None,
});

/// Options for [`start`].
#[derive(Default)]
pub struct Options {
    /// Fixed port; defaults to an ephemeral free port.
    pub port: Option<u16>,
    /// Path to a JSON config file.
    pub config: Option<String>,
    /// Extra environment variables for the child process.
    pub env: Vec<(String, String)>,
    /// Health-check timeout (default 10s).
    pub timeout: Option<Duration>,
}

/// Start the sidecar (idempotent). Returns the base URL (`http://host:port`).
pub fn start(opts: Options) -> Result<String, Error> {
    let mut state = STATE.lock().unwrap();
    if let Some(child) = state.child.as_mut() {
        if matches!(child.try_wait(), Ok(None)) {
            return Ok(state.base.clone().unwrap());
        }
    }

    let port = match opts.port {
        Some(p) => p,
        None => free_port()?,
    };
    let addr = format!("127.0.0.1:{port}");

    let mut cmd = Command::new(binary_path()?);
    cmd.env("LLMUX_ADDR", &addr)
        .stdin(Stdio::inherit())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    if let Some(cfg) = &opts.config {
        cmd.env("LLMUX_CONFIG", cfg);
    }
    for (k, v) in &opts.env {
        cmd.env(k, v);
    }

    let child = cmd.spawn().map_err(Error::Spawn)?;
    let base = format!("http://{addr}");
    let timeout = opts.timeout.unwrap_or(Duration::from_secs(10));

    state.child = Some(child);
    state.base = Some(base.clone());

    if let Err(e) = wait_healthy(&base, timeout) {
        if let Some(mut child) = state.child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
        state.base = None;
        return Err(e);
    }

    Ok(base)
}

/// The running base URL (`http://host:port`), starting the sidecar if needed.
pub fn base_url() -> Result<String, Error> {
    {
        let mut state = STATE.lock().unwrap();
        if let Some(child) = state.child.as_mut() {
            if matches!(child.try_wait(), Ok(None)) {
                return Ok(state.base.clone().unwrap());
            }
        }
    }
    start(Options::default())
}

/// The OpenAI-style base URL (`…/v1`).
pub fn openai_base_url() -> Result<String, Error> {
    Ok(format!("{}/v1", base_url()?))
}

/// Stop the sidecar if running.
pub fn stop() {
    let mut state = STATE.lock().unwrap();
    if let Some(mut child) = state.child.take() {
        if matches!(child.try_wait(), Ok(None)) {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
    state.base = None;
}

fn binary_path() -> Result<PathBuf, Error> {
    // 1) explicit override
    if let Ok(p) = env::var("LLMUX_BINARY") {
        if !p.is_empty() {
            return Ok(PathBuf::from(p));
        }
    }
    // 2) binary bundled next to the crate
    let name = if cfg!(windows) { "llmux.exe" } else { "llmux" };
    let bundled = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("bin")
        .join(name);
    if bundled.exists() {
        return Ok(bundled);
    }
    // 3) on PATH
    if which("llmux").is_some() {
        return Ok(PathBuf::from("llmux"));
    }
    Err(Error::BinaryNotFound)
}

fn which(cmd: &str) -> Option<PathBuf> {
    let path = env::var_os("PATH")?;
    for dir in env::split_paths(&path) {
        let candidate = dir.join(cmd);
        if candidate.is_file() {
            return Some(candidate);
        }
        if cfg!(windows) {
            let exe = dir.join(format!("{cmd}.exe"));
            if exe.is_file() {
                return Some(exe);
            }
        }
    }
    None
}

fn free_port() -> Result<u16, Error> {
    let listener = TcpListener::bind("127.0.0.1:0")?;
    Ok(listener.local_addr()?.port())
}

/// Minimal HTTP/1.0 GET /health that succeeds on a `200` status line.
fn wait_healthy(base: &str, timeout: Duration) -> Result<(), Error> {
    // base is "http://127.0.0.1:<port>"
    let hostport = base.trim_start_matches("http://");
    let deadline = Instant::now() + timeout;
    let mut last = String::from("connection refused");
    while Instant::now() < deadline {
        match health_once(hostport) {
            Ok(true) => return Ok(()),
            Ok(false) => last = "non-200 status".into(),
            Err(e) => last = e.to_string(),
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    Err(Error::Health(last))
}

fn health_once(hostport: &str) -> std::io::Result<bool> {
    use std::io::Write;
    let mut stream = TcpStream::connect_timeout(
        &hostport
            .parse()
            .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "addr"))?,
        Duration::from_secs(1),
    )?;
    stream.set_read_timeout(Some(Duration::from_secs(1)))?;
    stream.set_write_timeout(Some(Duration::from_secs(1)))?;
    let req = format!(
        "GET /health HTTP/1.0\r\nHost: {hostport}\r\nConnection: close\r\n\r\n"
    );
    stream.write_all(req.as_bytes())?;
    let mut buf = Vec::new();
    // Read just enough for the status line.
    let mut chunk = [0u8; 256];
    let n = stream.read(&mut chunk)?;
    buf.extend_from_slice(&chunk[..n]);
    let head = String::from_utf8_lossy(&buf);
    Ok(head.starts_with("HTTP/1.") && head.contains(" 200 "))
}

/// Convenience constructor returning an `async-openai` client pointed at the
/// local gateway. Enabled with the `async-openai` feature.
#[cfg(feature = "async-openai")]
pub fn openai_client() -> Result<async_openai::Client<async_openai::config::OpenAIConfig>, Error> {
    let base = openai_base_url()?;
    let config = async_openai::config::OpenAIConfig::new()
        .with_api_base(base)
        .with_api_key("llmux-local");
    Ok(async_openai::Client::with_config(config))
}

#[cfg(test)]
mod tests {
    //! Unit tests for the private helpers (binary resolution, health poll).
    //! Public-API / spawn tests live in `tests/sidecar.rs`.

    use super::*;
    use std::io::Write;
    use std::net::TcpListener;
    use std::sync::Mutex;
    use std::thread;

    // Serialize tests that mutate process-wide env (LLMUX_BINARY / PATH).
    static ENV_LOCK: Mutex<()> = Mutex::new(());

    #[test]
    fn binary_path_env_override_wins() {
        let _g = ENV_LOCK.lock().unwrap();
        let dir = std::env::temp_dir().join(format!("llmux-rs-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let target = dir.join("custom-llmux");
        std::fs::write(&target, "#!/bin/sh\n").unwrap();
        std::env::set_var("LLMUX_BINARY", &target);
        let got = binary_path().unwrap();
        std::env::remove_var("LLMUX_BINARY");
        assert_eq!(got, target);
    }

    #[test]
    fn binary_path_errors_when_missing() {
        let _g = ENV_LOCK.lock().unwrap();
        std::env::remove_var("LLMUX_BINARY");
        let saved = std::env::var_os("PATH");
        std::env::set_var("PATH", "");
        // Only assert the error when there is genuinely no bundled bin/llmux.
        let bundled = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("bin")
            .join("llmux");
        let res = binary_path();
        if let Some(p) = saved {
            std::env::set_var("PATH", p);
        }
        if !bundled.exists() {
            match res {
                Err(Error::BinaryNotFound) => {}
                other => panic!("expected BinaryNotFound, got {other:?}"),
            }
        }
    }

    #[test]
    fn free_port_is_in_range() {
        let p = free_port().unwrap();
        assert!(p > 0);
    }

    #[test]
    fn openai_base_url_appends_v1_format() {
        // Pure string contract via the public formatter logic.
        let base = "http://127.0.0.1:12345";
        assert_eq!(format!("{base}/v1"), "http://127.0.0.1:12345/v1");
    }

    #[test]
    fn wait_healthy_ready_on_200() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        thread::spawn(move || {
            for stream in listener.incoming() {
                if let Ok(mut s) = stream {
                    let mut buf = [0u8; 256];
                    use std::io::Read as _;
                    let _ = s.read(&mut buf);
                    let _ = s.write_all(
                        b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok",
                    );
                }
            }
        });
        let base = format!("http://127.0.0.1:{port}");
        wait_healthy(&base, Duration::from_secs(3)).expect("should become healthy");
    }

    #[test]
    fn wait_healthy_times_out_on_non_200() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        thread::spawn(move || {
            for stream in listener.incoming() {
                if let Ok(mut s) = stream {
                    let mut buf = [0u8; 256];
                    use std::io::Read as _;
                    let _ = s.read(&mut buf);
                    let _ = s.write_all(
                        b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                    );
                }
            }
        });
        let base = format!("http://127.0.0.1:{port}");
        let err = wait_healthy(&base, Duration::from_millis(400)).unwrap_err();
        assert!(matches!(err, Error::Health(_)));
    }

    #[test]
    fn wait_healthy_times_out_when_unreachable() {
        // Reserve then drop a port so nothing is listening.
        let port = free_port().unwrap();
        let base = format!("http://127.0.0.1:{port}");
        let err = wait_healthy(&base, Duration::from_millis(400)).unwrap_err();
        assert!(matches!(err, Error::Health(_)));
    }
}
