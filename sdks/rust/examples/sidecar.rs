//! llmux as a child process, over HTTP.
//!
//! ```text
//! cargo run --example sidecar                 # needs provider keys in the env
//! ../../sdks/rust/examples/run.sh sidecar     # offline, against ffi/fakeupstream
//! ```
//!
//! Environment:
//!   LLMUX_BINARY  path to the llmux binary (else `bin/llmux` or PATH)
//!   LLMUX_CONFIG  path to an llmux.json for the child
//!   LLMUX_MODEL   model to route (default "demo")
//!
//! The crate starts and supervises the process for you, so the user never runs
//! a server by hand. Compare with `examples/direct.rs`, which is the better
//! default for a Rust program — see the crate README for when it is not.

use std::process::ExitCode;
use std::time::{Duration, Instant};

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("error: {e}");
            // `llmux::stop()` also runs below on the happy path; calling it here
            // is what keeps a failed request from leaving a serving llmux
            // holding a port. Rust has no `defer`, so an error path that owns a
            // process has to say so explicitly.
            llmux::stop();
            ExitCode::FAILURE
        }
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let model = std::env::var("LLMUX_MODEL").unwrap_or_else(|_| "demo".to_string());

    // Idempotent: start() spawns the child on a free loopback port and blocks
    // until /health answers 200, or kills what it started and returns an error.
    let base = llmux::start(llmux::Options {
        config: std::env::var("LLMUX_CONFIG").ok(),
        timeout: Some(Duration::from_secs(15)),
        ..Default::default()
    })?;
    println!("sidecar: {base}");
    println!("openai:  {}", llmux::openai_base_url()?);

    let timeout = Duration::from_secs(60);

    // ------------------------------------------------------------------ models
    let t = Instant::now();
    let models = llmux::http::get(&format!("{base}/v1/models"), timeout)?;
    println!("models:  {} bytes in {:?}", models.len(), t.elapsed());

    // -------------------------------------------------------------------- chat
    // Byte for byte the same request document the C ABI takes. One wire
    // contract, two transports — that is the point of the JSON-in/JSON-out ABI.
    let req = format!(
        r#"{{"model":{model},"messages":[{{"role":"user","content":"count to four"}}]}}"#,
        model = json_string(&model)
    );
    let t = Instant::now();
    let chat = llmux::http::post_json(&format!("{base}/v1/chat/completions"), &req, timeout)?;
    println!("chat:    {:?}", t.elapsed());
    println!("         {}", first_line(&chat, 240));

    // ------------------------------------------------------------------ stream
    // SSE, not a C callback. This is the honest streaming path for any host
    // that cannot or will not take a callback across the FFI boundary — and it
    // is real streaming: the first chunk is printed before the last arrives.
    let streaming_req = format!(
        r#"{{"model":{model},"messages":[{{"role":"user","content":"hi"}}],"stream":true}}"#,
        model = json_string(&model)
    );
    print!("stream:  ");
    let t = Instant::now();
    let mut first_token: Option<Duration> = None;
    let n = llmux::http::post_sse(
        &format!("{base}/v1/chat/completions"),
        &streaming_req,
        timeout,
        |chunk| {
            if first_token.is_none() {
                first_token = Some(t.elapsed());
            }
            print!("{}", delta_content(chunk));
            true
        },
    )?;
    println!();
    println!(
        "         {n} chunks, first at {:?}, total {:?}",
        first_token.unwrap_or_default(),
        t.elapsed()
    );

    // ------------------------------------------------------------ error path
    let bad = r#"{"model":"no-such-model-anywhere","messages":[{"role":"user","content":"x"}]}"#;
    match llmux::http::post_json(&format!("{base}/v1/chat/completions"), bad, timeout) {
        Ok(_) => println!("bogus:   unexpectedly succeeded"),
        // llmux returns an OpenAI-shaped error object in the body. The HTTP
        // client surfaces it verbatim rather than paraphrasing a status code.
        Err(e) => println!("bogus:   {}", first_line(&e.to_string(), 200)),
    }

    // Stop the child. The crate also stops it when the process exits, but an
    // example that starts a server and does not stop it teaches the wrong
    // habit — the RAII equivalent here is calling stop() on both paths, which
    // main() does.
    llmux::stop();
    println!("stopped: child reaped");
    Ok(())
}

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
