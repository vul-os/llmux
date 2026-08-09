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

    Ok(())
    // gw drops here: llmux_close(handle).
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
