//! Integration tests for the direct (C ABI) path, against the REAL shared
//! library.
//!
//! Every test here is gated on `libllmux` actually being present: without it
//! they `return` rather than fail, because a checkout that has not run
//! `scripts/build-ffi.sh` is a normal state and not a broken one.
//!
//! Gating creates the classic false green — a suite that skips everything and
//! reports success — so `gate_is_honest_about_skipping` prints exactly which
//! way it went. Run with `--nocapture` to see it.
//!
//! Most tests here need no upstream at all — loading, the version probe,
//! handle lifetime, error strings, and the `models` method, which llmux
//! answers from memory. Tests that need a working upstream for a full "chat"
//! or "stream" call additionally need a fake. Two exist for two different
//! reasons:
//!
//!   - `ffi/fakeupstream`, the Go side's fake, is driven by `examples/run.sh`,
//!     not from here: standing up a Go binary from a Rust test would make
//!     `cargo test` depend on a Go toolchain.
//!   - `sdks/fake-upstream.py` is stdlib Python with no build step, so the
//!     cancellation tests below spawn it directly — they need to know how
//!     many chunks the upstream actually produced, which only that fake
//!     counts. They skip loudly (not silently) if `python3` is not on PATH,
//!     the same convention `library()`'s callers use for a missing
//!     `libllmux`.

use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::Duration;

use llmux::direct::{Error, Gateway};

/// The library, or `None` if this checkout has not built one.
fn library() -> Option<PathBuf> {
    llmux::direct::find_library().ok()
}

/// A running `sdks/fake-upstream.py`: the base URL it printed, plus the child
/// so the caller can kill it.
///
/// This is the counting fake, not `ffi/fakeupstream` — it exists specifically
/// to answer "how many chunks did the provider actually produce," which is
/// the whole point of a cancellation test. It is stdlib Python with no build
/// step, so unlike standing up a Go binary this does not push `cargo test`
/// onto a Go toolchain; it only needs `python3` on PATH, which
/// `tests/sidecar.rs` already assumes elsewhere in this same suite.
struct FakeUpstream {
    child: Child,
    url: String,
    config_json: String,
}

impl Drop for FakeUpstream {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

/// Starts `sdks/fake-upstream.py` with the given per-chunk delay, or `None` if
/// `python3` is not on PATH — callers skip loudly rather than failing, the
/// same convention `library()` and its callers use for a missing `libllmux`.
fn fake_upstream(text: &str, chunk_delay_ms: u32) -> Option<FakeUpstream> {
    if Command::new("python3").arg("--version").output().is_err() {
        return None;
    }
    let script = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("fake-upstream.py");
    let mut child = Command::new("python3")
        .arg(&script)
        .arg("--text")
        .arg(text)
        .arg("--chunk-delay-ms")
        .arg(chunk_delay_ms.to_string())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .expect("spawn sdks/fake-upstream.py");

    // The three lines are printed and flushed before the server starts
    // accepting connections (see fake-upstream.py's main()), so reading them
    // synchronously here cannot race the requests this test makes next.
    let stdout = child.stdout.take().expect("piped stdout");
    let mut lines = BufReader::new(stdout).lines();
    let url = lines
        .next()
        .expect("URL line")
        .expect("read URL line")
        .strip_prefix("URL ")
        .expect("URL-prefixed line")
        .to_string();
    let config_json = lines
        .next()
        .expect("CONFIG line")
        .expect("read CONFIG line")
        .strip_prefix("CONFIG ")
        .expect("CONFIG-prefixed line")
        .to_string();

    Some(FakeUpstream {
        child,
        url,
        config_json,
    })
}

/// Pulls an integer field out of the fake's flat `{"generated": N, ...}` JSON
/// without pulling serde_json into this crate's dev-dependencies for one
/// test. Panics on malformed input, which a hand-rolled fixture never emits.
fn json_uint_field(json: &str, field: &str) -> u64 {
    let needle = format!("\"{field}\":");
    let i = json.find(&needle).unwrap_or_else(|| panic!("no {field:?} in {json}"));
    let rest = json[i + needle.len()..].trim_start();
    let digits: String = rest.chars().take_while(|c| c.is_ascii_digit()).collect();
    digits
        .parse()
        .unwrap_or_else(|_| panic!("no digits after {field:?} in {json}"))
}

#[test]
fn gate_is_honest_about_skipping() {
    match library() {
        Some(p) => println!("libllmux found at {} — direct tests RAN", p.display()),
        None => println!("no libllmux — direct tests SKIPPED (run scripts/build-ffi.sh)"),
    }
}

#[test]
fn opens_reports_a_version_and_closes() {
    let Some(path) = library() else { return };
    let gw = Gateway::open_at(&path, None).expect("open");
    let v = gw.abi_version();
    assert!(!v.is_empty(), "abi_version returned an empty string");
    // Looks like a semver-ish version rather than, say, an error message.
    assert!(
        v.chars().next().is_some_and(|c| c.is_ascii_digit()),
        "unexpected version {v:?}"
    );
    drop(gw); // llmux_close
}

#[test]
fn version_mismatch_is_detected() {
    let Some(_) = library() else { return };
    let err = Gateway::open_checked("0.0.0-not-a-real-version", None).unwrap_err();
    match err {
        Error::VersionMismatch { loaded, expected } => {
            assert_eq!(expected, "0.0.0-not-a-real-version");
            assert!(!loaded.is_empty());
        }
        other => panic!("expected VersionMismatch, got {other:?}"),
    }
}

#[test]
fn models_is_answered_from_memory() {
    let Some(path) = library() else { return };
    let gw = Gateway::open_at(&path, None).expect("open");
    // No upstream is configured and none is needed: `models` is a local answer.
    let json = gw.call("models", None).expect("models");
    assert!(json.contains("\"object\":\"list\""), "unexpected: {json}");
}

#[test]
fn unknown_method_is_a_clean_error_not_a_crash() {
    let Some(path) = library() else { return };
    let gw = Gateway::open_at(&path, None).expect("open");
    let err = gw.call("definitely-not-a-method", Some("{}")).unwrap_err();
    let msg = err.to_string();
    assert!(msg.contains("unknown method"), "unexpected message: {msg}");
    // The message was malloc'd by the library and freed by Error::from_c. If it
    // were not, this test would still pass — which is exactly why leak checking
    // belongs in the ffi/ C smoke test, not here. See the README.
}

/// Regression test. This is the test that found the bug described on `Api`.
///
/// The first version of the direct module owned the `libloading::Library`
/// per-gateway and let it drop, so each cycle here was a fresh `dlopen` of a Go
/// `c-shared` object followed by a `dlclose`. This loop **hung**: iterations got
/// progressively slower and the process had to be killed. Loading once per
/// process and never unloading takes it to ~20 ms.
///
/// It also covers the thing it was originally written for: handles are never
/// reused, so a host that opens and closes in a loop consumes registry keys.
#[test]
fn many_open_close_cycles_do_not_exhaust_the_registry() {
    let Some(path) = library() else { return };
    for _ in 0..200 {
        let gw = Gateway::open_at(&path, None).expect("open");
        let _ = gw.call("models", None).expect("models");
        // dropped here: llmux_close, but NOT dlclose
    }
}

#[test]
fn interior_nul_is_rejected_before_it_reaches_c() {
    let Some(path) = library() else { return };
    let gw = Gateway::open_at(&path, None).expect("open");
    let err = gw.call("models", Some("{\0}")).unwrap_err();
    assert!(matches!(err, Error::Nul(_)), "got {err:?}");
}

#[test]
fn cancel_with_nothing_in_flight_is_a_no_op() {
    let Some(path) = library() else { return };
    let gw = Gateway::open_at(&path, None).expect("open");
    // Fact from the shared cancellation brief: cancelling an unknown handle,
    // or one with nothing running, is a no-op. So is cancelling twice. Neither
    // needs an upstream: "models" is answered from memory.
    gw.cancel();
    gw.cancel();
    let _ = gw
        .call("models", None)
        .expect("the handle must still work after a cancel with nothing in flight");
}

/// The bug `llmux_cancel` fixes, reproduced against the real counting fake.
///
/// Dropping [`llmux::direct::ChunkStream`] early — here, `break`ing a `for`
/// loop after 3 chunks — must reach `llmux_cancel` before it does anything
/// else, or llmux keeps fetching from upstream for however long it takes the
/// consumer's early exit to be noticed. `sdks/fake-upstream.py` counts chunks
/// at the point they are written to the socket, specifically so this can be
/// measured rather than assumed.
#[test]
fn dropping_the_iterator_early_reaches_llmux_cancel() {
    let Some(path) = library() else { return };
    let Some(up) = fake_upstream("one two three four five six seven eight nine ten", 100) else {
        eprintln!("skipping: python3 not available for sdks/fake-upstream.py");
        return;
    };

    let gw = Gateway::open_at(&path, Some(&up.config_json)).expect("open");
    let req = r#"{"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}"#;

    let mut seen = 0usize;
    {
        let stream = gw.stream(req).expect("stream");
        for chunk in stream {
            chunk.expect("the consumer that cancels must not see its own cancel as an Err");
            seen += 1;
            if seen == 3 {
                break; // ChunkStream::drop runs here: llmux_cancel first.
            }
        }
    }
    assert_eq!(seen, 3, "test setup: consumer should have taken exactly 3 chunks");

    // The handle survives the cancel: a fresh call on it still works. If
    // `llmux_cancel` had instead been wired to `llmux_close`, this would fail.
    let _ = gw
        .call("models", None)
        .expect("the handle must survive a cancelled stream");

    let generated_json =
        llmux::http::get(&format!("{}/generated", up.url), Duration::from_secs(2))
            .expect("GET /generated");
    let generated = json_uint_field(&generated_json, "generated");
    println!("consumer saw {seen} chunks; upstream {generated_json}");

    // Verified on this machine (see the shared cancellation brief): a stream
    // cancelled after 3 delivered chunks costs the upstream exactly 3
    // generated chunks. Asserting the exact figure, not just "less than a
    // full run," is the point: the whole bug was an off-by-a-few silently
    // rounding up to "close enough."
    assert_eq!(
        generated, 3,
        "cancel should stop upstream generation at exactly the delivered count"
    );
}

/// [`llmux::direct::CancelHandle`] cancelling from another thread while the
/// consumer is still pulling the iterator on this one. This is the shape the
/// idiomatic construct exists for: a watchdog thread, a `ctrlc` handler, a
/// `tokio::select!` arm — none of which hold the `Gateway` or the iterator.
///
/// Also proves the safety-review requirement directly: the consumer's `for`
/// loop must never see `Err("context canceled")` even though that is exactly
/// what the raw ABI call underneath returns — a stream this consumer asked to
/// be cancelled must not come back looking like a failure.
#[test]
fn cancel_handle_from_another_thread_ends_the_iterator_without_an_error() {
    let Some(path) = library() else { return };
    let Some(up) = fake_upstream("one two three four five six seven eight nine ten", 100) else {
        eprintln!("skipping: python3 not available for sdks/fake-upstream.py");
        return;
    };

    let gw = Gateway::open_at(&path, Some(&up.config_json)).expect("open");
    let cancel = gw.cancel_handle();
    let req = r#"{"model":"demo","messages":[{"role":"user","content":"hi"}],"stream":true}"#;

    let watchdog = std::thread::spawn(move || {
        // Comfortably inside the 10-chunk, 100ms-per-chunk run, and cancelled
        // from a thread that never touches `gw` or the iterator at all.
        std::thread::sleep(Duration::from_millis(350));
        cancel.cancel();
    });

    let mut seen = 0usize;
    for chunk in gw.stream(req).expect("stream") {
        match chunk {
            Ok(_) => seen += 1,
            Err(e) => panic!(
                "a deliberate cancel must not surface as an error to the consumer: {e}"
            ),
        }
    }
    watchdog.join().expect("watchdog thread panicked");

    assert!(
        (1..10).contains(&seen),
        "expected the stream to be cut short by the watchdog, got {seen} of 10"
    );

    // The handle survives here too, cancelled from a thread that was never
    // the one blocked inside llmux_stream.
    let _ = gw
        .call("models", None)
        .expect("the handle must survive a cancel issued from another thread");
}
