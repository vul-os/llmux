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
//! Tests that need a working upstream additionally need the fake, which the Go
//! side owns. They are driven by `examples/run.sh`, not from here: standing up
//! a Go binary from a Rust test would make `cargo test` depend on a Go
//! toolchain. What IS tested here is everything that needs no upstream —
//! loading, the version probe, handle lifetime, error strings, and the
//! `models` method, which llmux answers from memory.

use std::path::PathBuf;

use llmux::direct::{Error, Gateway};

/// The library, or `None` if this checkout has not built one.
fn library() -> Option<PathBuf> {
    llmux::direct::find_library().ok()
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
