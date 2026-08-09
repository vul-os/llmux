//! A deliberately tiny HTTP/1.1 client for talking to the sidecar.
//!
//! This crate has **no HTTP dependency**, and that is a decision rather than an
//! oversight. The sidecar surface an SDK needs is two shapes — POST JSON and
//! read one JSON body, and POST JSON and read an SSE stream — against
//! `127.0.0.1`. Pulling in `reqwest` for that would drag `hyper`, `tokio` and a
//! TLS stack into every consumer of a crate whose main job is to `dlopen` a
//! shared library.
//!
//! What you are looking at is therefore **not** a general HTTP client and must
//! not be used as one. It speaks to a loopback address, does no TLS, no
//! redirects, no proxies, no chunked-transfer decoding beyond what the sidecar
//! emits, and no connection reuse. If your program already depends on
//! `reqwest`, use `reqwest` — the wire contract is ordinary JSON over HTTP and
//! nothing here is privileged.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::Duration;

/// A failure talking to the sidecar.
#[derive(Debug)]
pub enum HttpError {
    /// The URL was not `http://host:port/path`.
    BadUrl(String),
    /// Connect, read or write failed.
    Io(std::io::Error),
    /// A non-200 response. Carries the status line and the body, because the
    /// body is where llmux puts its OpenAI-shaped error object.
    Status(u16, String),
}

impl std::fmt::Display for HttpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HttpError::BadUrl(u) => write!(f, "not an http:// url: {u}"),
            HttpError::Io(e) => write!(f, "io: {e}"),
            HttpError::Status(c, b) => write!(f, "HTTP {c}: {b}"),
        }
    }
}

impl std::error::Error for HttpError {}

impl From<std::io::Error> for HttpError {
    fn from(e: std::io::Error) -> Self {
        HttpError::Io(e)
    }
}

fn split_url(url: &str) -> Result<(String, String), HttpError> {
    let rest = url
        .strip_prefix("http://")
        .ok_or_else(|| HttpError::BadUrl(url.to_string()))?;
    match rest.find('/') {
        Some(i) => Ok((rest[..i].to_string(), rest[i..].to_string())),
        None => Ok((rest.to_string(), "/".to_string())),
    }
}

fn connect(hostport: &str, timeout: Duration) -> Result<TcpStream, HttpError> {
    let stream = TcpStream::connect(hostport)?;
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))?;
    Ok(stream)
}

fn send(
    stream: &mut TcpStream,
    method: &str,
    hostport: &str,
    path: &str,
    accept: &str,
    body: Option<&str>,
) -> Result<(), HttpError> {
    let mut req = format!(
        "{method} {path} HTTP/1.1\r\nHost: {hostport}\r\nAccept: {accept}\r\nConnection: close\r\n"
    );
    if let Some(b) = body {
        req.push_str("Content-Type: application/json\r\n");
        req.push_str(&format!("Content-Length: {}\r\n", b.len()));
    }
    req.push_str("\r\n");
    stream.write_all(req.as_bytes())?;
    if let Some(b) = body {
        stream.write_all(b.as_bytes())?;
    }
    stream.flush()?;
    Ok(())
}

/// Reads the status line and headers, leaving the reader positioned at the
/// body. Returns the status code.
fn read_head<R: BufRead>(r: &mut R) -> Result<u16, HttpError> {
    let mut line = String::new();
    r.read_line(&mut line)?;
    let code = line
        .split_whitespace()
        .nth(1)
        .and_then(|c| c.parse::<u16>().ok())
        .ok_or_else(|| HttpError::BadUrl(format!("bad status line: {}", line.trim())))?;
    loop {
        let mut h = String::new();
        if r.read_line(&mut h)? == 0 || h == "\r\n" || h == "\n" {
            break;
        }
    }
    Ok(code)
}

/// GET a URL and return the body, erroring on any non-200.
pub fn get(url: &str, timeout: Duration) -> Result<String, HttpError> {
    request("GET", url, None, timeout)
}

/// POST a JSON body and return the response body, erroring on any non-200.
pub fn post_json(url: &str, body: &str, timeout: Duration) -> Result<String, HttpError> {
    request("POST", url, Some(body), timeout)
}

fn request(
    method: &str,
    url: &str,
    body: Option<&str>,
    timeout: Duration,
) -> Result<String, HttpError> {
    let (hostport, path) = split_url(url)?;
    let mut stream = connect(&hostport, timeout)?;
    send(
        &mut stream,
        method,
        &hostport,
        &path,
        "application/json",
        body,
    )?;

    let mut reader = BufReader::new(&mut stream);
    let code = read_head(&mut reader)?;
    let mut out = String::new();
    reader.read_to_string(&mut out)?;
    if code != 200 {
        return Err(HttpError::Status(code, out.trim().to_string()));
    }
    Ok(out)
}

/// POST a JSON body and stream the `text/event-stream` response, calling
/// `on_chunk` with each `data:` payload and stopping at `[DONE]`.
///
/// Return `true` from `on_chunk` to keep reading and `false` to stop. Returns
/// the number of chunks delivered.
///
/// This is the sidecar's answer to `llmux_stream`, and the reason a host that
/// cannot or will not load a shared library still has an honest streaming path
/// rather than one that buffers the answer and replays it as fake chunks.
pub fn post_sse<F>(
    url: &str,
    body: &str,
    timeout: Duration,
    mut on_chunk: F,
) -> Result<usize, HttpError>
where
    F: FnMut(&str) -> bool,
{
    let (hostport, path) = split_url(url)?;
    let mut stream = connect(&hostport, timeout)?;
    send(
        &mut stream,
        "POST",
        &hostport,
        &path,
        "text/event-stream",
        Some(body),
    )?;

    let mut reader = BufReader::new(&mut stream);
    let code = read_head(&mut reader)?;
    if code != 200 {
        let mut out = String::new();
        reader.read_to_string(&mut out)?;
        return Err(HttpError::Status(code, out.trim().to_string()));
    }

    let mut n = 0usize;
    let mut line = String::new();
    loop {
        line.clear();
        if reader.read_line(&mut line)? == 0 {
            break;
        }
        let line = line.trim();
        let Some(payload) = line.strip_prefix("data:") else {
            continue;
        };
        let payload = payload.trim();
        if payload == "[DONE]" {
            break;
        }
        n += 1;
        if !on_chunk(payload) {
            break;
        }
    }
    Ok(n)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn split_url_splits_host_and_path() {
        assert_eq!(
            split_url("http://127.0.0.1:8080/v1/models").unwrap(),
            ("127.0.0.1:8080".to_string(), "/v1/models".to_string())
        );
        assert_eq!(
            split_url("http://127.0.0.1:8080").unwrap(),
            ("127.0.0.1:8080".to_string(), "/".to_string())
        );
    }

    #[test]
    fn split_url_rejects_https() {
        // Refusing loudly beats connecting to port 443 and speaking plaintext.
        assert!(matches!(
            split_url("https://example.com/"),
            Err(HttpError::BadUrl(_))
        ));
    }

    #[test]
    fn read_head_parses_status_and_consumes_headers() {
        let raw = b"HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\n\r\nbody here";
        let mut r = BufReader::new(&raw[..]);
        assert_eq!(read_head(&mut r).unwrap(), 404);
        let mut rest = String::new();
        r.read_to_string(&mut rest).unwrap();
        assert_eq!(rest, "body here");
    }
}
