//! Direct mode: llmux running **inside this process**, over the C ABI.
//!
//! This module `dlopen`s `libllmux` and binds the seven functions in
//! [`ffi/include/llmux.h`]. There is no server, no port and no loopback socket;
//! a call is a C call into Go with JSON in and JSON out.
//!
//! ```no_run
//! let gw = llmux::direct::Gateway::open(None)?;      // config: None = defaults + env
//! let res = gw.call("chat", Some(r#"{"model":"demo","messages":[
//!     {"role":"user","content":"hi"}]}"#))?;
//! for chunk in gw.stream(r#"{"model":"demo","messages":[
//!     {"role":"user","content":"hi"}],"stream":true}"#)? {
//!     print!("{}", chunk?);
//! }
//! # Ok::<(), llmux::direct::Error>(())
//! ```
//!
//! # Read this before you choose it
//!
//! Loading this library loads **the Go runtime** into your process: its garbage
//! collector, its scheduler, and its signal handlers for `SIGSEGV`, `SIGBUS`,
//! `SIGFPE` and `SIGPROF`. It is **not fork-safe** — after `fork()` without
//! `exec()` the runtime in the child is broken. And it is a **12–17 MB
//! artifact** that exists prebuilt only for darwin/arm64 and linux/arm64.
//!
//! For most Rust programs those are acceptable, because Rust does not pre-fork
//! and does not embed a competing runtime. For a Rust program that does — one
//! that forks workers itself, or that links a JVM or a sanitizer — the sidecar
//! in [`crate`] is the supported answer, not a fallback.
//!
//! **Latency is not the reason to embed.** Measured in `ffi/bench`: the
//! boundary is ~4 µs in-process against ~46 µs over loopback, and a whole chat
//! call is ~80–92 µs against ~102–109 µs. A real model answers in hundreds of
//! milliseconds. The reasons are no second process, no port to bind, and no
//! loopback surface to secure.
//!
//! # Memory and handles
//!
//! Every `char*` the library returns — results *and* error messages — is freed
//! with `llmux_free` and nothing else. Every path in this module does that,
//! including the error paths: [`Error::from_c`] takes ownership of the message,
//! copies it, and frees it before returning. Handles are closed by [`Gateway`]'s
//! `Drop`, so a `?` anywhere in your code cannot leak one.
//!
//! # Cancellation
//!
//! `break`ing out of a `for chunk in gw.stream(...)?` loop — or dropping the
//! [`ChunkStream`] any other way — is what a Rust user means by "I do not want
//! the rest of this stream," and it reaches `llmux_cancel` for you: see
//! [`ChunkStream`]'s `Drop`. [`Gateway::cancel_handle`] hands out a
//! `Clone + Send + Sync + 'static` [`CancelHandle`] for cancelling from
//! somewhere that does not hold the iterator at all — a `ctrlc` handler, a
//! `tokio::select!` arm, a watchdog thread. [`Gateway::cancel`] is the same
//! operation without detaching a handle, for callers driving [`Gateway::call`]
//! or [`Gateway::stream_with`] directly.
//!
//! `llmux_cancel` is **per-handle, not per-call**: it aborts every call in
//! flight on the gateway, not just the one you meant. If you need independent
//! cancellation scopes — two streams where cancelling one must not touch the
//! other — give each scope its own [`Gateway::open`].

use std::ffi::{c_char, c_int, c_void, CStr, CString};
use std::fmt;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/// Everything that can go wrong on the direct path.
#[derive(Debug)]
pub enum Error {
    /// No `libllmux` could be found. Carries the paths that were tried.
    LibraryNotFound(Vec<PathBuf>),
    /// `dlopen` failed, or a symbol was missing — usually a library built from
    /// a different llmux, or one that is not llmux at all.
    Load(libloading::Error),
    /// The loaded library reports a different version than expected.
    VersionMismatch { loaded: String, expected: String },
    /// llmux itself failed. The string is the library's own message, which is
    /// plain UTF-8 text and deliberately **not** JSON — do not parse it.
    Llmux(String),
    /// A Rust string could not be passed to C because it contains a NUL.
    Nul(std::ffi::NulError),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::LibraryNotFound(tried) => {
                write!(
                    f,
                    "libllmux not found. Set LLMUX_LIBRARY to its path, or build it with \
                     `scripts/build-ffi.sh`. Tried: "
                )?;
                for (i, p) in tried.iter().enumerate() {
                    if i > 0 {
                        write!(f, ", ")?;
                    }
                    write!(f, "{}", p.display())?;
                }
                Ok(())
            }
            Error::Load(e) => write!(f, "loading libllmux: {e}"),
            Error::VersionMismatch { loaded, expected } => write!(
                f,
                "libllmux reports version {loaded}, expected {expected} — a stale library is \
                 earlier on your load path"
            ),
            Error::Llmux(m) => write!(f, "{m}"),
            Error::Nul(e) => write!(f, "argument contains an interior NUL: {e}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<libloading::Error> for Error {
    fn from(e: libloading::Error) -> Self {
        Error::Load(e)
    }
}

impl From<std::ffi::NulError> for Error {
    fn from(e: std::ffi::NulError) -> Self {
        Error::Nul(e)
    }
}

impl Error {
    /// Takes ownership of a `char*` error message from the library, copies it
    /// into a Rust `String`, and **frees it**. Passing the message through
    /// `Error` without this step is the classic C-ABI binding leak.
    ///
    /// # Safety
    /// `err` must be either null or a pointer the library returned.
    unsafe fn from_c(api: &Api, err: *mut c_char) -> Error {
        if err.is_null() {
            return Error::Llmux("llmux failed without a message".into());
        }
        let msg = CStr::from_ptr(err).to_string_lossy().into_owned();
        (api.free)(err);
        Error::Llmux(msg)
    }

    /// True if this is the specific failure a blocked `call` / `stream` /
    /// `stream_with` returns when `llmux_cancel` lands on it — the library's
    /// exact message is `"context canceled"` (see `ffi/include/llmux.h`).
    ///
    /// [`ChunkStream`] already checks this for you and ends the iteration
    /// silently instead of handing it to you as an `Err` — abandoning an
    /// iterator is not a failure the consumer asked to see. This is for
    /// [`Gateway::call`] and [`Gateway::stream_with`], which return the raw
    /// error because they have no iterator-shaped place to swallow it, and
    /// which would otherwise force you to string-match `"context canceled"`
    /// yourself to tell a deliberate cancel apart from an ordinary upstream
    /// failure.
    pub fn is_cancelled(&self) -> bool {
        matches!(self, Error::Llmux(m) if m == "context canceled")
    }
}

/// Shorthand for this module's results.
pub type Result<T> = std::result::Result<T, Error>;

// ---------------------------------------------------------------------------
// The ABI
// ---------------------------------------------------------------------------

type AbiVersionFn = unsafe extern "C" fn() -> *const c_char;
type NewFn = unsafe extern "C" fn(*const c_char, *mut *mut c_char) -> u64;
type CloseFn = unsafe extern "C" fn(u64);
type CancelFn = unsafe extern "C" fn(u64);
type CallFn =
    unsafe extern "C" fn(u64, *const c_char, *const c_char, *mut *mut c_char) -> *mut c_char;
type FreeFn = unsafe extern "C" fn(*mut c_char);
type ChunkCb = unsafe extern "C" fn(*const c_char, *mut c_void) -> c_int;
type StreamFn = unsafe extern "C" fn(
    u64,
    *const c_char,
    *const c_char,
    ChunkCb,
    *mut c_void,
    *mut *mut c_char,
) -> c_int;

/// The loaded library and its seven resolved symbols.
///
/// # Why this is never dropped
///
/// `Api` deliberately has no `Drop`, is only ever handed out as `&'static`, and
/// the [`libloading::Library`] inside it is **leaked on purpose**.
///
/// `libllmux` is a Go `c-shared` object. Loading it starts the Go runtime and
/// its threads, and those threads keep running for the life of the process —
/// there is no "shut the Go runtime down" entry point, because Go does not have
/// one. `dlclose` would unmap the code those threads are executing.
///
/// This is not theoretical. An earlier version of this module put the `Library`
/// in an `Arc` alongside the handle and let it drop, and a test that opened and
/// closed 200 gateways in a loop **hung**: each iteration re-ran the library's
/// initialisers, each cycle took longer than the last, and the process
/// eventually stopped making progress and had to be killed. The fix is the
/// design below — one load per path per process, cached, never unloaded.
///
/// The cost is one leaked mapping per distinct library path, which is bounded
/// by how many libraries you load and is normally one.
struct Api {
    abi_version: AbiVersionFn,
    new: NewFn,
    close: CloseFn,
    cancel: CancelFn,
    call: CallFn,
    free: FreeFn,
    stream: StreamFn,
    /// Kept so the mapping outlives the pointers above. Never dropped.
    _lib: libloading::Library,
}

// SAFETY: the fields are C function pointers (Send + Sync by nature) plus a
// libloading::Library, which is itself Send + Sync. The ABI documents every
// entry point as safe to call from multiple threads.
unsafe impl Send for Api {}
unsafe impl Sync for Api {}

/// Libraries loaded by this process, by path. Entries are `&'static` and are
/// never removed — see [`Api`].
static LOADED: Mutex<Vec<(PathBuf, &'static Api)>> = Mutex::new(Vec::new());

impl Api {
    /// Returns the process-wide `Api` for `path`, loading it on first use.
    ///
    /// Repeat calls with the same path return the same `&'static Api` without
    /// touching `dlopen` again.
    fn shared(path: &Path) -> Result<&'static Api> {
        let mut loaded = LOADED.lock().unwrap_or_else(|e| e.into_inner());
        if let Some((_, api)) = loaded.iter().find(|(p, _)| p == path) {
            return Ok(api);
        }
        // SAFETY: loading arbitrary code from `path` is inherently unsafe; the
        // caller chose the path. The initialisers run here, once.
        let api = unsafe { Api::load(path)? };
        // Leak: see the type-level comment. This is the whole point.
        let api: &'static Api = Box::leak(Box::new(api));
        loaded.push((path.to_path_buf(), api));
        Ok(api)
    }

    unsafe fn load(path: &Path) -> Result<Api> {
        let lib = libloading::Library::new(path)?;
        // Copy each fn pointer out of its borrowing Symbol. The pointers stay
        // valid because `_lib` below owns the mapping and it is never dropped.
        let abi_version = *lib.get::<AbiVersionFn>(b"llmux_abi_version\0")?;
        let new = *lib.get::<NewFn>(b"llmux_new\0")?;
        let close = *lib.get::<CloseFn>(b"llmux_close\0")?;
        let cancel = *lib.get::<CancelFn>(b"llmux_cancel\0")?;
        let call = *lib.get::<CallFn>(b"llmux_call\0")?;
        let free = *lib.get::<FreeFn>(b"llmux_free\0")?;
        let stream = *lib.get::<StreamFn>(b"llmux_stream\0")?;
        Ok(Api {
            abi_version,
            new,
            close,
            cancel,
            call,
            free,
            stream,
            _lib: lib,
        })
    }

    fn version(&self) -> String {
        // SAFETY: llmux_abi_version returns a static string owned by the
        // library. It must NOT be freed — it is the one exception to the
        // ownership rule, and freeing it here would be a heap corruption bug
        // that only shows up under a different allocator.
        unsafe {
            CStr::from_ptr((self.abi_version)())
                .to_string_lossy()
                .into_owned()
        }
    }
}

// ---------------------------------------------------------------------------
// Locating the library
// ---------------------------------------------------------------------------

/// The platform's file name for the shared library.
///
/// Windows is listed for completeness only. **No `llmux.dll` has ever been
/// built** — see this crate's README.
pub fn library_file_name() -> &'static str {
    if cfg!(target_os = "windows") {
        "llmux.dll"
    } else if cfg!(target_os = "macos") {
        "libllmux.dylib"
    } else {
        "libllmux.so"
    }
}

/// Finds `libllmux`, in this order:
///
/// 1. `$LLMUX_LIBRARY`, if set — an explicit path always wins.
/// 2. `dist/ffi/<goos>_<goarch>/` walking up from the crate, which is where
///    `scripts/build-ffi.sh` puts it in a checkout.
/// 3. The bare file name, which hands the decision to the platform loader
///    (`DYLD_LIBRARY_PATH`, `LD_LIBRARY_PATH`, the default search path).
pub fn find_library() -> Result<PathBuf> {
    let name = library_file_name();
    let mut tried = Vec::new();

    if let Ok(p) = std::env::var("LLMUX_LIBRARY") {
        if !p.is_empty() {
            let p = PathBuf::from(p);
            if p.exists() {
                return Ok(p);
            }
            tried.push(p);
        }
    }

    let goos = if cfg!(target_os = "macos") {
        "darwin"
    } else if cfg!(target_os = "windows") {
        "windows"
    } else {
        "linux"
    };
    let goarch = if cfg!(target_arch = "aarch64") {
        "arm64"
    } else {
        "amd64"
    };
    let rel = format!("dist/ffi/{goos}_{goarch}/{name}");

    let mut dir: Option<&Path> = Some(Path::new(env!("CARGO_MANIFEST_DIR")));
    while let Some(d) = dir {
        let candidate = d.join(&rel);
        if candidate.exists() {
            return Ok(candidate);
        }
        tried.push(candidate);
        dir = d.parent();
    }

    // Let the loader try. If it is on the default search path this works; if
    // not, `dlopen` fails with a message naming the file, which is a better
    // error than anything this function could invent.
    let bare = PathBuf::from(name);
    if std::env::var_os("LLMUX_LIBRARY").is_none() && tried.len() > 1 {
        tried.push(bare.clone());
    }
    // SAFETY: probing whether the loader can resolve the bare name. This runs
    // the library's initialisers, which for libllmux means starting the Go
    // runtime — the same thing `open` is about to do deliberately.
    if unsafe { libloading::Library::new(&bare) }.is_ok() {
        return Ok(bare);
    }
    Err(Error::LibraryNotFound(tried))
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

/// A gateway handle plus the library it belongs to.
///
/// Split out from [`Gateway`] so a stream worker thread can hold an owning
/// `Arc` of it: the handle cannot be closed while a stream is still running on
/// it, no matter what the caller does with the `Gateway` or the iterator.
struct Inner {
    api: &'static Api,
    handle: u64,
}

impl Drop for Inner {
    fn drop(&mut self) {
        // SAFETY: `handle` came from llmux_new and is closed exactly once.
        // llmux_close is documented idempotent and aborts any stream still
        // running on the handle, so this is safe even in a panic unwind.
        unsafe { (self.api.close)(self.handle) }
    }
}

/// An llmux gateway running in this process.
///
/// Closing is [`Drop`]: the handle is released when the `Gateway` goes out of
/// scope, on the early-return and panic paths as much as the happy one. There
/// is no `close()` to forget to call.
///
/// A `Gateway` is `Send` and `Sync` — the ABI documents a handle as safe to use
/// from several threads at once.
pub struct Gateway {
    inner: Arc<Inner>,
}

impl fmt::Debug for Gateway {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Gateway")
            .field("handle", &self.inner.handle)
            .field("abi_version", &self.inner.api.version())
            .finish()
    }
}

impl Gateway {
    /// Loads the library (see [`find_library`]) and creates a gateway.
    ///
    /// `config_json` is an llmux configuration document — the same JSON
    /// `llmux serve` reads from `llmux.json`. `None` means built-in defaults
    /// plus the environment (auto-detected providers, `LLMUX_*` overrides).
    ///
    /// Construction is **inert**: no goroutines start and, unless the config
    /// names a Postgres DSN, no sockets open.
    pub fn open(config_json: Option<&str>) -> Result<Gateway> {
        Self::open_at(&find_library()?, config_json)
    }

    /// [`Gateway::open`] against a library at a known path.
    pub fn open_at(path: &Path, config_json: Option<&str>) -> Result<Gateway> {
        Self::from_api(Api::shared(path)?, config_json)
    }

    /// [`Gateway::open`] with a version check first.
    ///
    /// Worth doing at startup. A shared library resolves off a load path you
    /// may not control, and a stale `libllmux` earlier on it is otherwise
    /// called silently and misbehaves in ways that look like llmux bugs.
    pub fn open_checked(expected_version: &str, config_json: Option<&str>) -> Result<Gateway> {
        let api = Api::shared(&find_library()?)?;
        let loaded = api.version();
        if loaded != expected_version {
            return Err(Error::VersionMismatch {
                loaded,
                expected: expected_version.to_string(),
            });
        }
        Self::from_api(api, config_json)
    }

    fn from_api(api: &'static Api, config_json: Option<&str>) -> Result<Gateway> {
        let cfg = config_json.map(CString::new).transpose()?;
        let cfg_ptr = cfg.as_ref().map_or(std::ptr::null(), |c| c.as_ptr());

        let mut err: *mut c_char = std::ptr::null_mut();
        // SAFETY: cfg_ptr is null or a valid NUL-terminated string that
        // outlives the call; err is a valid out-parameter.
        let handle = unsafe { (api.new)(cfg_ptr, &mut err) };
        if handle == 0 {
            return Err(unsafe { Error::from_c(api, err) });
        }
        Ok(Gateway {
            inner: Arc::new(Inner { api, handle }),
        })
    }

    /// The version the loaded library was built from, e.g. `"0.1.2"`.
    pub fn abi_version(&self) -> String {
        self.inner.api.version()
    }

    /// One unary call. `method` is `"chat"`, `"embed"` or `"models"`; the
    /// request and the result are the same JSON the HTTP API uses.
    ///
    /// A `"chat"` request with `"stream": true` is **refused** rather than
    /// quietly served as one blob — use [`Gateway::stream`].
    pub fn call(&self, method: &str, request_json: Option<&str>) -> Result<String> {
        let m = CString::new(method)?;
        let req = request_json.map(CString::new).transpose()?;
        let req_ptr = req.as_ref().map_or(std::ptr::null(), |c| c.as_ptr());

        let mut err: *mut c_char = std::ptr::null_mut();
        // SAFETY: handle is open (we own it), the two strings outlive the call.
        let out =
            unsafe { (self.inner.api.call)(self.inner.handle, m.as_ptr(), req_ptr, &mut err) };
        if out.is_null() {
            return Err(unsafe { Error::from_c(self.inner.api, err) });
        }
        // Copy, then free. The library allocated `out` with Go's allocator; it
        // must go back through llmux_free and never through Rust's.
        let s = unsafe { CStr::from_ptr(out).to_string_lossy().into_owned() };
        unsafe { (self.inner.api.free)(out) };
        Ok(s)
    }

    /// Aborts every call in flight on this handle, without closing it — the
    /// handle stays open and the next call starts on a fresh context.
    ///
    /// This is the low-level primitive. [`Gateway::stream`]'s iterator already
    /// calls this for you when it is dropped early (see [`ChunkStream`]);
    /// reach for this directly only when you are driving [`Gateway::call`] or
    /// [`Gateway::stream_with`] yourself and need to abandon a call those
    /// methods have no other way to interrupt. [`Gateway::cancel_handle`]
    /// gives you the same operation detached from `&Gateway`, for a thread
    /// that has no other business touching the gateway.
    ///
    /// Safe to call from another thread while `call`, `stream` or
    /// `stream_with` is blocked on this same handle — that is the only way to
    /// abandon one, since none of them poll for cancellation on their own.
    /// Also safe from *inside* a chunk callback, unlike [`Gateway`]'s `Drop`
    /// (`llmux_close`), which must never be called from a callback because it
    /// waits for the very call running it. A no-op if nothing is in flight, if
    /// you call it twice, or if the handle is unknown or already closed — so
    /// there is no wrong time to call it.
    ///
    /// The blocked call returns an error whose message is exactly
    /// `"context canceled"` (see [`Error::is_cancelled`]); chunks already
    /// delivered stay delivered, and tokens already served are metered either
    /// way. [`Gateway::call`] and [`Gateway::stream_with`] hand that error
    /// back to you as an ordinary `Err` because they have no iterator-shaped
    /// place to swallow it; [`ChunkStream`] does have one and never surfaces
    /// it.
    ///
    /// **Per handle, not per call.** This aborts *every* call currently
    /// running on this gateway, not just one you had in mind. A gateway
    /// shared by two concurrent streams cannot cancel only one of them — give
    /// each independent cancellation scope its own [`Gateway::open`].
    pub fn cancel(&self) {
        // SAFETY: llmux_cancel treats an unknown or already-closed handle as a
        // clean no-op (see ffi/include/llmux.h), so there is no handle-validity
        // precondition beyond `self` existing, which borrowck already
        // guarantees.
        unsafe { (self.inner.api.cancel)(self.inner.handle) }
    }

    /// Detaches a [`CancelHandle`] for this gateway: the same operation as
    /// [`Gateway::cancel`], but `Clone + Send + Sync + 'static` so it can be
    /// handed to a `ctrlc` handler, a `tokio::select!` arm, or a watchdog
    /// thread that has no other reason to hold — or outlive — a `&Gateway`.
    pub fn cancel_handle(&self) -> CancelHandle {
        CancelHandle {
            api: self.inner.api,
            handle: self.inner.handle,
        }
    }

    /// Streams a chat completion, calling `on_chunk` once per chunk on **this**
    /// thread, and blocking until the stream ends.
    ///
    /// Return `true` from `on_chunk` to keep streaming and `false` to stop.
    /// Stopping early is not an error — you returned `false`, so you already
    /// know it happened — and this returns `Ok(())`. Tokens already served are
    /// metered either way.
    ///
    /// Prefer [`Gateway::stream`] unless you specifically want the callback
    /// form; the iterator is the idiomatic Rust shape.
    pub fn stream_with<F>(&self, request_json: &str, mut on_chunk: F) -> Result<()>
    where
        F: FnMut(&str) -> bool,
    {
        // A Rust panic unwinding across the C frame into Go is undefined
        // behaviour, so the trampoline catches it and stops the stream instead.
        unsafe extern "C" fn trampoline<F: FnMut(&str) -> bool>(
            chunk: *const c_char,
            user: *mut c_void,
        ) -> c_int {
            let f = &mut *(user as *mut F);
            let text = if chunk.is_null() {
                ""
            } else {
                CStr::from_ptr(chunk).to_str().unwrap_or("")
            };
            match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| f(text))) {
                Ok(true) => 0,
                // Either the caller asked to stop, or the callback panicked.
                // Both mean "stop the stream", and a non-zero return is the
                // documented, non-error way to do that.
                Ok(false) | Err(_) => 1,
            }
        }

        let m = CString::new("chat")?;
        let req = CString::new(request_json)?;
        let mut err: *mut c_char = std::ptr::null_mut();
        let user = &mut on_chunk as *mut F as *mut c_void;

        // SAFETY: `user` points at `on_chunk`, which outlives this call because
        // llmux_stream is documented to block until the stream ends and to
        // invoke the callback synchronously on this thread.
        let rc = unsafe {
            (self.inner.api.stream)(
                self.inner.handle,
                m.as_ptr(),
                req.as_ptr(),
                trampoline::<F>,
                user,
                &mut err,
            )
        };
        if rc != 0 {
            return Err(unsafe { Error::from_c(self.inner.api, err) });
        }
        Ok(())
    }

    /// Streams a chat completion as an [`Iterator`] of chunk JSON documents.
    ///
    /// ```no_run
    /// # let gw = llmux::direct::Gateway::open(None)?;
    /// for chunk in gw.stream(r#"{"model":"demo","messages":[],"stream":true}"#)? {
    ///     println!("{}", chunk?);
    /// }
    /// # Ok::<(), llmux::direct::Error>(())
    /// ```
    ///
    /// # How this works, and why it costs a thread
    ///
    /// `llmux_stream` is a **blocking call with a callback**; an `Iterator` is
    /// a pull API. Bridging the two needs somewhere for the blocking call to
    /// live, so this spawns one worker thread and hands chunks across a
    /// **rendezvous channel** (`sync_channel(0)`).
    ///
    /// The zero capacity is the load-bearing part: it means nothing is
    /// buffered. The worker blocks inside the callback until you take the
    /// chunk, so time-to-first-token is real and backpressure is real. A
    /// binding that collected the whole answer and replayed it as chunks would
    /// look identical until someone measured it.
    ///
    /// The iterator owns an `Arc` of the handle (via the worker thread's own
    /// clone), so the gateway cannot be closed underneath it. Dropping the
    /// iterator early — a `break`, an early `?`, a panic — reaches
    /// `llmux_cancel` before anything else: see [`ChunkStream`]'s `Drop` for
    /// why that ordering is the fix for a real bug, not a nicety.
    pub fn stream(&self, request_json: &str) -> Result<ChunkStream> {
        let (tx, rx): (SyncSender<Result<String>>, Receiver<Result<String>>) = sync_channel(0);
        let inner = Arc::clone(&self.inner);
        let req = request_json.to_string();

        let worker = std::thread::Builder::new()
            .name("llmux-stream".into())
            .spawn(move || {
                let gw = Gateway { inner };
                let send_tx = tx.clone();
                let res = gw.stream_with(&req, move |chunk| {
                    // A send error means the consumer dropped the iterator.
                    // Returning false stops the stream, which is the documented
                    // non-error way out.
                    send_tx.send(Ok(chunk.to_string())).is_ok()
                });
                if let Err(e) = res {
                    // A cancellation this SDK itself asked for (directly via
                    // Gateway::cancel/CancelHandle, or indirectly because
                    // ChunkStream::drop got there first) is not a failure the
                    // consumer asked to see — it is the mechanism working.
                    // Swallowing it here is what makes the iterator end
                    // silently (the next `recv` sees a disconnected sender)
                    // instead of yielding one last `Err("context canceled")`
                    // after the consumer already walked away, or worse, to a
                    // CancelHandle user who cancelled deliberately and would
                    // otherwise see their own cancellation reported as a bug.
                    // A genuine failure is not swallowed: only this exact,
                    // documented sentinel is.
                    if !e.is_cancelled() {
                        // Best effort: if the consumer has gone, there is
                        // nobody to tell, and that is fine too.
                        let _ = tx.send(Err(e));
                    }
                }
            })
            .expect("spawn llmux-stream worker");

        Ok(ChunkStream {
            rx,
            worker: Some(worker),
            cancel: self.cancel_handle(),
        })
    }
}

/// A detached handle for cancelling a [`Gateway`] from somewhere that does not
/// hold the gateway, or even the same thread as the code driving it.
///
/// `Clone + Send + Sync + 'static` on purpose: those are exactly the bounds a
/// `ctrlc` handler, a `tokio::select!` arm, or a watchdog thread need to be
/// handed one. Get one from [`Gateway::cancel_handle`]; calling
/// [`CancelHandle::cancel`] does what [`Gateway::cancel`] does.
///
/// # Why holding this can never be unsound
///
/// This carries the resolved `&'static Api` — never dropped, see [`Api`]'s
/// type-level comment — and the bare `u64` handle. Nothing else. In
/// particular it does **not** hold an `Arc<Inner>`, so cloning it and
/// outliving the [`Gateway`] it came from does not keep the underlying handle
/// open: drop every `Gateway` and [`ChunkStream`] that references the handle
/// and `llmux_close` runs on schedule, exactly as if no `CancelHandle` existed
/// to hold it back. And a `CancelHandle` that outlives the close is not a
/// use-after-free waiting to happen: handles are never reused, and the ABI
/// documents `llmux_cancel` on an unknown or already-closed handle as a clean
/// no-op (see `ffi/include/llmux.h`). So there is no window in this type's
/// lifetime where calling `cancel()` is unsound — at worst, after the gateway
/// has closed, it does nothing.
#[derive(Clone, Copy)]
pub struct CancelHandle {
    api: &'static Api,
    handle: u64,
}

// SAFETY: `Api`'s fields are C function pointers (safe to call from any
// thread, per the ABI's own documentation) plus a `libloading::Library` that
// is itself Send + Sync and never unloaded (see `Api`'s `unsafe impl`s above);
// `u64` is plain data. Sending or sharing a `CancelHandle` across threads adds
// no new hazard beyond what `Api` already allows.
unsafe impl Send for CancelHandle {}
unsafe impl Sync for CancelHandle {}

impl fmt::Debug for CancelHandle {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("CancelHandle")
            .field("handle", &self.handle)
            .finish()
    }
}

impl CancelHandle {
    /// Aborts every call in flight on the gateway this handle came from,
    /// without closing it. See [`Gateway::cancel`] for the full contract —
    /// this is that method with the `Gateway` detached so it can cross a
    /// thread boundary that otherwise has no reason to touch the gateway at
    /// all.
    pub fn cancel(&self) {
        // SAFETY: same as Gateway::cancel — llmux_cancel accepts any u64,
        // including a closed or unknown one, and is documented to treat that
        // as a no-op.
        unsafe { (self.api.cancel)(self.handle) }
    }
}

/// An iterator over streaming chat chunks. See [`Gateway::stream`].
///
/// Each item is one `chat.completion.chunk` JSON document — byte for byte what
/// the HTTP API writes after `data: `, without the prefix and without the
/// terminal `[DONE]` frame.
///
/// Never yields `Err` for a cancellation — its own, or one delivered through a
/// [`CancelHandle`] from another thread while this iterator is still being
/// pulled elsewhere. Either way the iteration just ends (the next call to
/// `next` returns `None`), the same as if the upstream had finished normally.
/// Abandoning a stream is not a failure a Rust consumer asked to see; a real
/// upstream or transport failure is still `Err`, unaffected by any of this.
pub struct ChunkStream {
    rx: Receiver<Result<String>>,
    worker: Option<JoinHandle<()>>,
    /// Detached handle for this stream's gateway. Used by `Drop`, and kept as
    /// a `CancelHandle` rather than the `Arc<Inner>` the worker thread already
    /// holds so that this field can never be the thing keeping the handle
    /// open — see `CancelHandle`'s own doc comment.
    cancel: CancelHandle,
}

impl Iterator for ChunkStream {
    type Item = Result<String>;

    fn next(&mut self) -> Option<Self::Item> {
        self.rx.recv().ok()
    }
}

impl Drop for ChunkStream {
    /// The fix for a real bug: a consumer that reads 3 of 10 chunks and then
    /// walks away — a `break`, an early `?`, a panic — used to only stop the
    /// *next* chunk from reaching them. llmux had already started fetching the
    /// one after that from upstream (the callback for chunk N returns before
    /// llmux issues the blocking read for chunk N+1), and that fetch ran to
    /// completion and was metered before the closed channel was ever noticed.
    /// Measured against `sdks/fake-upstream.py`: cancelling after 3 delivered
    /// chunks costs the upstream exactly 3 generated chunks with the call
    /// below in place, not 4 — see the crate README for the full numbers.
    ///
    /// Ordering is load-bearing, and matches the safety note on
    /// `llmux_stream`'s callback: the trampoline's `user_data` points at a
    /// closure living on the *worker* thread's stack, and it stays valid for
    /// as long as that thread's `llmux_stream` call has not returned. So:
    ///
    /// 1. Cancel FIRST, while that call may still be blocked inside Go. This
    ///    is what makes it a prompt abort of whatever chunk llmux is already
    ///    reading, rather than a wait for the next chunk boundary.
    /// 2. Only THEN close the receiving end. Belt and suspenders: cancel is
    ///    the fast path that stops upstream generation, but if the worker
    ///    happens to be sitting in `send_tx.send` with a chunk already in hand
    ///    rather than blocked in the native call, it still needs to see that
    ///    nobody will ever drain that channel again.
    /// 3. Only THEN join the worker. This is what actually waits for the
    ///    native call to unwind back out of Go and the worker thread to exit
    ///    — i.e. for the point up to which `user_data` needed to stay valid —
    ///    before this function (and the `Arc<Inner>` clone the worker's local
    ///    `Gateway` held) can return and be released. Dropping the channel or
    ///    releasing the handle before this join would risk the callback
    ///    touching memory that is no longer there.
    fn drop(&mut self) {
        self.cancel.cancel();

        let (dead_tx, dead_rx) = sync_channel(0);
        drop(dead_tx);
        let _ = std::mem::replace(&mut self.rx, dead_rx);
        if let Some(w) = self.worker.take() {
            let _ = w.join();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn library_file_name_matches_this_platform() {
        let name = library_file_name();
        if cfg!(target_os = "macos") {
            assert_eq!(name, "libllmux.dylib");
        } else if cfg!(target_os = "windows") {
            assert_eq!(name, "llmux.dll");
        } else {
            assert_eq!(name, "libllmux.so");
        }
    }

    #[test]
    fn missing_library_names_what_it_tried() {
        // A path that cannot exist, so this asserts the error text rather than
        // whether a library happens to be present on the machine.
        let err = Gateway::open_at(Path::new("/nonexistent/libllmux.dylib"), None).unwrap_err();
        assert!(matches!(err, Error::Load(_)), "got {err:?}");
    }

    #[test]
    fn error_display_lists_tried_paths() {
        let e = Error::LibraryNotFound(vec![PathBuf::from("/a/libllmux.so")]);
        let s = e.to_string();
        assert!(s.contains("LLMUX_LIBRARY"), "{s}");
        assert!(s.contains("/a/libllmux.so"), "{s}");
    }

    /// `Error::is_cancelled` matches on the library's exact wording and
    /// nothing else — a looser match (e.g. "canceled" anywhere in the
    /// message) would also catch an unrelated upstream error that happens to
    /// mention cancellation, and a stricter one (e.g. requiring a specific
    /// `Error` variant) would not exist, because `llmux_cancel`'s failure is
    /// not distinguished from any other `Error::Llmux` at the ABI boundary.
    #[test]
    fn is_cancelled_matches_only_the_exact_message() {
        assert!(Error::Llmux("context canceled".to_string()).is_cancelled());
        assert!(!Error::Llmux("deadline exceeded".to_string()).is_cancelled());
        assert!(!Error::Llmux("context canceled: extra".to_string()).is_cancelled());
        assert!(!Error::Nul(std::ffi::CString::new("a\0b").unwrap_err()).is_cancelled());
    }

    /// `CancelHandle` must be `Clone + Send + Sync + 'static` to be handed to
    /// a `ctrlc` handler or a watchdog thread — that is the entire reason it
    /// exists rather than requiring callers to keep a `&Gateway` around. This
    /// is a compile-time assertion: it proves nothing at runtime and is not
    /// meant to.
    #[test]
    fn cancel_handle_is_send_sync_static() {
        fn assert_bounds<T: Clone + Send + Sync + 'static>() {}
        assert_bounds::<CancelHandle>();
    }
}
