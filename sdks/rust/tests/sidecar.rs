//! Public-API tests for the llmux Rust sidecar launcher.
//!
//! These drive the global singleton (`start`/`base_url`/`openai_base_url`/`stop`)
//! by pointing `LLMUX_BINARY` at a fake fixture (a tiny python HTTP server that
//! honors `LLMUX_ADDR` and serves `/health`). Because the launcher uses a
//! process-wide singleton, all the assertions live in a single test so they run
//! serially regardless of cargo's test threading.
//!
//! Set `LLMUX_BINARY` to the real gateway to additionally run an end-to-end
//! check.

use std::fs;
use std::io::Write;
use std::net::TcpStream;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::process::Command;
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

// All public-API tests share the process-wide singleton + LLMUX_BINARY env, so
// they must not run concurrently. cargo runs tests on parallel threads within a
// single process; this lock serializes them.
fn serial() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
    LOCK.get_or_init(|| Mutex::new(()))
        .lock()
        .unwrap_or_else(|p| p.into_inner())
}

fn fake_fixture() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests")
        .join("fixtures")
        .join("fake_llmux.py")
}

fn python3() -> Option<String> {
    for c in ["python3", "python"] {
        if Command::new(c).arg("--version").output().is_ok() {
            return Some(c.to_string());
        }
    }
    None
}

/// Write an executable shell wrapper that runs the python fake fixture.
fn make_fake(extra_env: &[(&str, &str)]) -> PathBuf {
    let py = python3().expect("python3 required for the fake fixture");
    let dir = std::env::temp_dir().join(format!(
        "llmux-rs-fake-{}-{}",
        std::process::id(),
        Instant::now().elapsed().as_nanos()
    ));
    fs::create_dir_all(&dir).unwrap();
    let wrapper = dir.join("llmux");
    let mut exports = String::new();
    for (k, v) in extra_env {
        exports.push_str(&format!("export {k}=\"{v}\"\n"));
    }
    let fixture = fake_fixture();
    fs::write(
        &wrapper,
        format!(
            "#!/bin/sh\n{exports}exec \"{py}\" \"{}\"\n",
            fixture.display()
        ),
    )
    .unwrap();
    let mut perms = fs::metadata(&wrapper).unwrap().permissions();
    perms.set_mode(0o755);
    fs::set_permissions(&wrapper, perms).unwrap();
    wrapper
}

fn port_open(port: u16) -> bool {
    TcpStream::connect_timeout(
        &format!("127.0.0.1:{port}").parse().unwrap(),
        Duration::from_millis(300),
    )
    .is_ok()
}

fn wait_port_closed(port: u16, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if !port_open(port) {
            return true;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    !port_open(port)
}

fn health_status(base: &str, status_target: u16) -> bool {
    let hostport = base.trim_start_matches("http://");
    if let Ok(mut s) = TcpStream::connect(hostport) {
        let _ = s.write_all(
            format!("GET /health HTTP/1.0\r\nHost: {hostport}\r\nConnection: close\r\n\r\n")
                .as_bytes(),
        );
        use std::io::Read as _;
        let mut buf = [0u8; 128];
        let n = s.read(&mut buf).unwrap_or(0);
        let head = String::from_utf8_lossy(&buf[..n]);
        return head.contains(&format!(" {status_target} "));
    }
    false
}

#[test]
fn sidecar_public_api_with_fake() {
    let _g = serial();
    if python3().is_none() {
        eprintln!("skipping: python3 not available for the fake fixture");
        return;
    }

    // 1) URL formatting + readiness on /health 200.
    std::env::set_var("LLMUX_BINARY", make_fake(&[]));
    let base = llmux::start(llmux::Options {
        timeout: Some(Duration::from_secs(10)),
        ..Default::default()
    })
    .expect("fake should become healthy");
    assert!(
        base.starts_with("http://127.0.0.1:"),
        "base = {base}"
    );
    let v1 = llmux::openai_base_url().unwrap();
    assert_eq!(v1, format!("{base}/v1"));
    assert!(v1.ends_with("/v1"));
    assert!(health_status(&base, 200));

    // 2) Singleton: start/base_url again -> same base, no respawn.
    let port: u16 = base.rsplit(':').next().unwrap().parse().unwrap();
    let again = llmux::start(llmux::Options::default()).unwrap();
    assert_eq!(again, base);
    assert_eq!(llmux::base_url().unwrap(), base);
    assert!(port_open(port));

    // 3) Cleanup: stop() kills the child and frees the port.
    llmux::stop();
    assert!(wait_port_closed(port, Duration::from_secs(3)), "port not freed");

    std::env::remove_var("LLMUX_BINARY");
}

#[test]
fn sidecar_times_out_when_never_200() {
    let _g = serial();
    if python3().is_none() {
        eprintln!("skipping: python3 not available");
        return;
    }
    llmux::stop();
    std::env::set_var("LLMUX_BINARY", make_fake(&[("FAKE_HEALTH_STATUS", "503")]));
    let err = llmux::start(llmux::Options {
        timeout: Some(Duration::from_millis(600)),
        ..Default::default()
    })
    .unwrap_err();
    assert!(matches!(err, llmux::Error::Health(_)), "got {err:?}");
    llmux::stop();
    std::env::remove_var("LLMUX_BINARY");
}

#[test]
fn integration_real_binary() {
    let _g = serial();
    let real = std::env::var("LLMUX_BINARY").ok().filter(|s| !s.is_empty());
    let bundled = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("bin")
        .join("llmux");
    let bin = real.or_else(|| {
        if bundled.exists() {
            Some(bundled.display().to_string())
        } else {
            None
        }
    });
    let Some(bin) = bin else {
        eprintln!("skipping integration: real llmux binary not available");
        return;
    };
    llmux::stop();
    std::env::set_var("LLMUX_BINARY", &bin);
    let base = llmux::start(llmux::Options {
        timeout: Some(Duration::from_secs(15)),
        ..Default::default()
    })
    .expect("real binary should become healthy");
    assert!(base.starts_with("http://127.0.0.1:"));
    assert!(health_status(&base, 200));
    assert!(llmux::openai_base_url().unwrap().ends_with("/v1"));
    llmux::stop();
}
