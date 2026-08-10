//! llmux in this process, over the C ABI.
//!
//! ```text
//! cargo run --example direct                  # needs provider keys in the env
//! ../../sdks/rust/examples/run.sh direct      # offline, against ffi/fakeupstream
//! ```
//!
//! Environment:
//!   LLMUX_LIBRARY      path to libllmux.dylib / .so (else it is searched for)
//!   LLMUX_CONFIG_JSON  an llmux config document; unset means defaults + env
//!   LLMUX_MODEL        model to route (default "demo")

use std::process::ExitCode;
use std::time::Instant;

use llmux::direct::{Error, Gateway};

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("error: {e}");
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), Error> {
    let config = std::env::var("LLMUX_CONFIG_JSON").ok();
    let model = std::env::var("LLMUX_MODEL").unwrap_or_else(|_| "demo".to_string());

    let path = llmux::direct::find_library()?;
    println!("library: {}", path.display());

    // `?` here would return without leaking anything: there is no handle yet.
    let gw = Gateway::open_at(&path, config.as_deref())?;
    // From here on the handle is owned by `gw`. It is released by Drop — at the
    // end of this function, on every `?` below, and on a panic. There is no
    // close() to forget, which is the whole reason to wrap a C handle in a Rust
    // type rather than exposing the u64.
    println!("abi:     {}", gw.abi_version());

    // In production, compare that against the version your bindings were
    // generated for and refuse to continue on a mismatch:
    //
    //     let gw = Gateway::open_checked("0.1.2", config.as_deref())?;
    //
    // A shared library resolves off a load path you may not control.

    // ------------------------------------------------------------------ models
    // Answered from memory — no upstream is contacted. This is the call the
    // ffi/ benchmark uses to measure the boundary itself (~4 µs in-process
    // against ~46 µs over loopback).
    let t = Instant::now();
    let models = gw.call("models", None)?;
    println!("models:  {} bytes in {:?}", models.len(), t.elapsed());
    println!("         {}", first_line(&models, 160));

    // -------------------------------------------------------------------- chat
    // The request and the response are the same JSON the HTTP API uses. A body
    // that works against POST /v1/chat/completions works here unchanged.
    let req = format!(
        r#"{{"model":{model},"messages":[{{"role":"user","content":"count to four"}}]}}"#,
        model = json_string(&model)
    );
    let t = Instant::now();
    let chat = gw.call("chat", Some(&req))?;
    println!("chat:    {:?}", t.elapsed());
    println!("         {}", first_line(&chat, 240));

    // A "chat" with "stream": true is REFUSED here rather than quietly served
    // as one blob after the fact. Demonstrating that beats describing it.
    let streaming_req = format!(
        r#"{{"model":{model},"messages":[{{"role":"user","content":"hi"}}],"stream":true}}"#,
        model = json_string(&model)
    );
    match gw.call("chat", Some(&streaming_req)) {
        Ok(_) => println!("refusal: NOT refused — that is a bug in llmux"),
        Err(e) => println!("refusal: {e}"),
    }

    // ------------------------------------------------------------------ stream
    // The idiomatic Rust shape: an Iterator. Under it is a worker thread and a
    // rendezvous channel, so nothing is buffered and time-to-first-token is
    // real. See Gateway::stream.
    print!("stream:  ");
    let t = Instant::now();
    let mut first_token: Option<std::time::Duration> = None;
    let mut chunks = 0usize;
    for chunk in gw.stream(&streaming_req)? {
        let chunk = chunk?; // dropping the iterator here would stop the stream
                            // and join the worker — no leak, no orphan thread.
        chunks += 1;
        if first_token.is_none() {
            first_token = Some(t.elapsed());
        }
        print!("{}", delta_content(&chunk));
    }
    println!();
    println!(
        "         {chunks} chunks, first at {:?}, total {:?}",
        first_token.unwrap_or_default(),
        t.elapsed()
    );

    // ------------------------------------------------------- early termination
    // Returning false from the callback stops the stream. It is NOT an error:
    // you returned false, so you already know it happened. Tokens already
    // served are metered either way.
    let mut seen = 0usize;
    gw.stream_with(&streaming_req, |_chunk| {
        seen += 1;
        seen < 2 // stop after the second chunk
    })?;
    println!("early:   stopped after {seen} chunk(s), Ok(()) — stopping is not a failure");

    // ------------------------------------------------------------ error path
    // An unknown method is a clean error string, not a crash, and the message
    // it allocated is freed by Error::from_c before this line runs.
    match gw.call("no-such-method", Some("{}")) {
        Ok(_) => println!("bogus:   unexpectedly succeeded"),
        Err(e) => println!("bogus:   {e}"),
    }

    // ------------------------------------------------------------ cancel
    // llmux 0.1.5 added `llmux_cancel`, the only way to abandon a blocked call
    // without destroying the whole gateway — that is what llmux_close does,
    // and llmux_close must never be called from inside a callback (it waits
    // for the very call running it). `Gateway::stream`'s iterator already
    // reaches it for you: dropping the iterator below, via `break`, is enough.
    //
    // This needs a fake that can prove something the `fakeupstream` this
    // example otherwise points at cannot: how many chunks the upstream
    // actually produced. `sdks/fake-upstream.py` counts a chunk the moment it
    // is written to a socket and serves the count at GET /generated, so this
    // section spawns its own copy of it rather than reusing `path`'s config.
    // "demo" here, not the outer `model`: this section stands up its own
    // fake-upstream.py with its own config, which always routes exactly
    // "demo" — not whatever LLMUX_MODEL happened to be set to for `gw` above.
    match run_cancellation_demo(&path, "demo") {
        Ok(()) => {}
        Err(msg) => println!("cancel:  skipped ({msg})"),
    }

    Ok(())
    // gw drops here: llmux_close(handle).
}

/// Streams from `sdks/fake-upstream.py` at 100ms/chunk, cancels after 3
/// delivered chunks by dropping the iterator, and prints what the consumer
/// saw against what the upstream actually generated.
///
/// Returns `Err` with a human-readable reason if `python3` is not available —
/// the same "skip loudly" convention `find_library` and its callers use for a
/// missing `libllmux` — rather than failing the whole example over a fixture
/// dependency.
fn run_cancellation_demo(lib_path: &std::path::Path, model: &str) -> std::result::Result<(), String> {
    use std::io::BufRead;
    use std::process::{Command, Stdio};

    if Command::new("python3").arg("--version").output().is_err() {
        return Err("python3 not on PATH; sdks/fake-upstream.py needs it".into());
    }

    let script = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("fake-upstream.py");
    let mut child = Command::new("python3")
        .arg(&script)
        .args(["--text", "one two three four five six seven eight nine ten"])
        .args(["--chunk-delay-ms", "100"])
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .map_err(|e| format!("spawn {}: {e}", script.display()))?;

    // Kills the fake on every return path below, including `?`. An example
    // that leaves a listening python process behind every time someone runs
    // it is exactly the kind of small leak this crate's own Gateway::Drop
    // exists to not have.
    struct KillOnDrop(std::process::Child);
    impl Drop for KillOnDrop {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let stdout = child.stdout.take().ok_or("fake-upstream.py had no stdout")?;
    let _guard = KillOnDrop(child);
    let mut lines = std::io::BufReader::new(stdout).lines();

    let url = lines
        .next()
        .ok_or("fake-upstream.py exited before printing URL")?
        .map_err(|e| e.to_string())?
        .strip_prefix("URL ")
        .ok_or("expected a URL-prefixed line")?
        .to_string();
    let config = lines
        .next()
        .ok_or("fake-upstream.py exited before printing CONFIG")?
        .map_err(|e| e.to_string())?
        .strip_prefix("CONFIG ")
        .ok_or("expected a CONFIG-prefixed line")?
        .to_string();

    // A second, independent gateway against the counting fake — not the `gw`
    // the rest of this example uses, so this section cannot disturb it.
    let cgw = Gateway::open_at(lib_path, Some(&config)).map_err(|e| e.to_string())?;
    let req = format!(
        r#"{{"model":{model},"messages":[{{"role":"user","content":"count to ten"}}],"stream":true}}"#,
        model = json_string(model)
    );

    let mut seen = 0usize;
    {
        let stream = cgw.stream(&req).map_err(|e| e.to_string())?;
        for chunk in stream {
            chunk.map_err(|e| e.to_string())?; // never "context canceled": we cancelled ourselves
            seen += 1;
            if seen == 3 {
                break; // ChunkStream::drop runs here: llmux_cancel, THEN teardown.
            }
        }
    }
    println!("cancel:  consumer saw {seen} chunks");

    // The handle survives: a fresh call on the SAME handle still works.
    let _ = cgw.call("models", None).map_err(|e| e.to_string())?;
    println!("cancel:  handle survives — a fresh call on it still works");

    let generated = llmux::http::get(
        &format!("{url}/generated"),
        std::time::Duration::from_secs(2),
    )
    .map_err(|e| e.to_string())?;
    println!("cancel:  GET /generated -> {generated}");

    Ok(())
}

/// Minimal JSON string quoting, so the example does not need serde to build a
/// two-field request. Anything real should use serde_json.
fn json_string(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Pulls `choices[0].delta.content` out of a chunk without a JSON parser. Fine
/// for an example; use serde_json in anything real.
fn delta_content(chunk: &str) -> String {
    let Some(i) = chunk.find(r#""content":""#) else {
        return String::new();
    };
    let rest = &chunk[i + r#""content":""#.len()..];
    let mut out = String::new();
    let mut escaped = false;
    for c in rest.chars() {
        if escaped {
            match c {
                'n' => out.push('\n'),
                't' => out.push('\t'),
                c => out.push(c),
            }
            escaped = false;
        } else if c == '\\' {
            escaped = true;
        } else if c == '"' {
            break;
        } else {
            out.push(c);
        }
    }
    out
}

fn first_line(s: &str, max: usize) -> String {
    let line = s.lines().next().unwrap_or("");
    if line.chars().count() <= max {
        return line.to_string();
    }
    let truncated: String = line.chars().take(max).collect();
    format!("{truncated}…")
}
