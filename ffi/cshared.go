package main

// The cgo shim. Everything here is marshalling: C strings in, C strings out,
// and one trampoline so Go can call the host's chunk callback. The behaviour
// lives in abi.go, in pure Go, where it can be read and tested without cgo.
//
// This file is implicitly constrained to CGO_ENABLED=1 builds (any file that
// imports "C" is), which is why abi.go carries `func main()` — the module still
// builds, vets and tests with cgo off.

/*
#include <stdint.h>
#include <stdlib.h>

// One chunk of a stream. Return 0 to continue, non-zero to stop.
typedef int (*llmux_chunk_cb)(const char* chunk_json, void* user_data);

// Go cannot call a C function pointer directly; it calls this instead. It must
// be `static inline`: cgo copies the preamble of an //export-carrying file into
// more than one generated translation unit, so a plain function definition here
// would be a duplicate symbol at link time.
static inline int llmux_invoke_chunk_cb(llmux_chunk_cb cb, const char* chunk_json, void* user_data) {
	return cb(chunk_json, user_data);
}
*/
import "C"

import "unsafe"

// abiVersionC is allocated once at load time and deliberately never freed:
// llmux_abi_version returns a `const char*` the caller must NOT pass to
// llmux_free, so it cannot be per-call malloc'd memory. One ~6-byte allocation
// per loaded library.
var abiVersionC = C.CString(Version)

// setErr writes a malloc'd copy of err's message through errp, if the caller
// supplied somewhere to put it. Callers free it with llmux_free, exactly like a
// result string — one free function for everything this library returns is the
// only rule a binding can be relied on to follow.
func setErr(errp **C.char, err error) {
	if errp == nil || err == nil {
		return
	}
	*errp = C.CString(err.Error())
}

// clearErr writes NULL through errp. Every fallible export calls it FIRST.
//
// Without it, *err was only ever written on failure, so a binding that declares
// one `char *err = NULL;` and reuses it across calls — the obvious way to write
// one, and what most of these bindings do — kept the message from an earlier
// failure after a later success. The binding then frees it a second time on its
// next error path, or on cleanup, and the double free lands in the HOST's
// allocator. Clearing on entry makes "*err is non-NULL" mean "THIS call failed",
// which is what every caller already assumed it meant.
//
// It does not free what was there: this library did not necessarily allocate it
// and, even if it did, freeing a pointer the host may still be holding would be
// the same bug pointed the other way. Ownership passed to the host when it was
// written; the host frees it with llmux_free.
func clearErr(errp **C.char) {
	if errp != nil {
		*errp = nil
	}
}

// goStr converts a possibly-NULL C string to Go. NULL becomes "", which the
// callers treat as "not supplied" — a host passing NULL for an optional
// argument must not be a crash.
func goStr(s *C.char) string {
	if s == nil {
		return ""
	}
	return C.GoString(s)
}

// Every //export below ends with a `defer` that recovers. That is the second
// half of the panic backstop: abi.go's entry points already recover (see
// `recovered` there), which is where every panic this library can actually
// produce is caught and where the behaviour is tested. The defers here cover
// the remaining sliver — a panic raised in the cgo marshalling itself, between
// the //export frame and the Go entry point: C.GoString on a pointer that is
// not NUL-terminated, C.CString on a Go string containing a NUL byte (it
// panics), an allocation failure. A panic that escapes an //export function is
// not a Go panic anyone can catch; it is a runtime fatal error that ends the
// host process.
//
// TestEveryExportHasAPanicBackstop reads this file and fails if an //export
// function is added without one.

// Returns the version of llmux this library was built from, as a static string
// the caller must not free. A host compares it against the version it generated
// its bindings for; a mismatch means a stale library is earlier on the load
// path than the one that was installed.
//
//export llmux_abi_version
func llmux_abi_version() (v *C.char) {
	defer func() {
		if rec := recover(); rec != nil {
			_ = rec
			v = nil
		}
	}()
	return abiVersionC
}

// Builds a gateway from a JSON configuration document (NULL or "" means
// defaults plus environment) and returns its handle. Returns 0 on failure with
// *err set to a malloc'd message.
//
//export llmux_new
func llmux_new(configJSON *C.char, err **C.char) (handle C.uint64_t) {
	clearErr(err)
	defer func() {
		if rec := recover(); rec != nil {
			handle = 0
			setErr(err, recovered(rec))
		}
	}()
	h, e := openGateway(goStr(configJSON))
	if e != nil {
		setErr(err, e)
		return 0
	}
	return C.uint64_t(h)
}

// Releases the gateway behind h and aborts any stream still running on it.
// Closing an unknown or already-closed handle is a no-op.
//
//export llmux_close
func llmux_close(h C.uint64_t) {
	defer func() { _ = recover() }() // void: nowhere to report, but never fatal
	closeGateway(uint64(h))
}

// Aborts every call currently in flight on h and leaves the handle open. Safe
// to call from another thread while a call is blocked, which is the point:
// llmux_call and llmux_stream block until they finish, and without this the
// only way to abandon one was llmux_close, which destroys the gateway.
// Cancelling an unknown handle, or one with nothing running, is a no-op.
//
//export llmux_cancel
func llmux_cancel(h C.uint64_t) {
	defer func() { _ = recover() }() // void: nowhere to report, but never fatal
	cancelGateway(uint64(h))
}

// One unary call. method is "chat", "embed" or "models"; requestJSON is the
// same OpenAI-shaped body the HTTP API takes. Returns malloc'd UTF-8 JSON the
// caller frees with llmux_free, or NULL with *err set.
//
//export llmux_call
func llmux_call(h C.uint64_t, method *C.char, requestJSON *C.char, err **C.char) (result *C.char) {
	clearErr(err)
	defer func() {
		if rec := recover(); rec != nil {
			// A panic in C.CString below can only have happened before the
			// pointer was assigned to result, so there is nothing half-built to
			// free here; NULL + *err is the ABI's documented failure.
			result = nil
			setErr(err, recovered(rec))
		}
	}()
	out, e := callMethod(uint64(h), goStr(method), goStr(requestJSON))
	if e != nil {
		setErr(err, e)
		return nil
	}
	return C.CString(out)
}

// Frees anything this library returned — results AND error strings. NULL is
// accepted. Do not pass it the return of llmux_abi_version.
//
//export llmux_free
func llmux_free(p *C.char) {
	defer func() { _ = recover() }() // void: nowhere to report, but never fatal
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

// Streams a chat completion, invoking cb once per chunk with that chunk's JSON.
// Blocks until the stream ends. Returns 0 on success — including when cb asked
// to stop by returning non-zero, which is the host's own decision and not an
// error — or -1 with *err set.
//
// The chunk JSON is owned by this library and is only valid for the duration of
// the callback: a host that needs it afterwards must copy it.
//
//export llmux_stream
func llmux_stream(h C.uint64_t, method *C.char, requestJSON *C.char,
	cb C.llmux_chunk_cb, userData unsafe.Pointer, err **C.char) (rc C.int) {
	clearErr(err)
	defer func() {
		if rec := recover(); rec != nil {
			rc = -1
			setErr(err, recovered(rec))
		}
	}()
	if cb == nil {
		setErr(err, errNoCallback)
		return -1
	}
	e := streamMethod(uint64(h), goStr(method), goStr(requestJSON), func(chunk string) error {
		cs := C.CString(chunk)
		defer C.free(unsafe.Pointer(cs))
		if stop := C.llmux_invoke_chunk_cb(cb, cs, userData); stop != 0 {
			return errCallbackStop
		}
		return nil
	})
	switch {
	case e == nil, abortedError(e):
		return 0
	default:
		setErr(err, e)
		return -1
	}
}
