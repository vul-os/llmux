package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
)

// Version is what llmux_abi_version reports. It is the llmux package version,
// and TestABIVersionMatchesThePackageVersion asserts it equals the repo's
// VERSION file — the point of the probe is that a host can compare the loaded
// library against the version it was built for and refuse a stale .so on the
// load path instead of calling into it and guessing.
const Version = "0.1.4"

// main exists because -buildmode=c-shared requires package main. It never runs:
// loading a c-shared library initialises the Go runtime and the package's init
// functions, and returns. Nothing here starts a goroutine, opens a socket, or
// installs anything at load time — constructing a gateway is llmux_new's job
// and even that is inert (core/gateway.New starts nothing; see embedtest/).
func main() {}

// ---------------------------------------------------------------------------
// Panic backstop
// ---------------------------------------------------------------------------

// recovered turns a recovered panic value into an ordinary error carrying the
// value and the stack it came from.
//
// Why every entry point needs this. In c-shared mode there is no goroutine
// above us to unwind into: a panic that escapes an //export function is a
// runtime fatal error, and it takes the HOST process with it — a Rails worker,
// a Python daemon, a JVM — printing a Go traceback its operator cannot act on.
// The HTTP shell has had this backstop from the start (server.recoverMW turns a
// panic into a logged 500), and the README calls the sidecar and the library
// interchangeable deployments of the same gateway. Without a recover here they
// are not interchangeable at all: the same bug is a 500 in one and a dead
// worker in the other.
//
// The panic is converted into the error channel the ABI already has — the
// trailing char** err — so a host learns what happened through the mechanism it
// already handles, and the stack goes with it because there is nowhere else for
// a shared library to put one (it owns no log destination in someone else's
// process).
func recovered(rec any) error {
	return fmt.Errorf("llmux: recovered from a panic inside the library: %v\n%s", rec, debug.Stack())
}

// newGateway builds the gateway behind a handle. It is a variable, not a direct
// call to gateway.New, so a test can substitute a constructor that panics and
// prove openGateway's recover backstop actually catches it. Nothing else
// reassigns it.
var newGateway = gateway.New

// ---------------------------------------------------------------------------
// Handle registry
// ---------------------------------------------------------------------------

// instance is one live gateway behind one handle.
//
// The context is swappable, not fixed for the life of the handle. llmux_call
// and llmux_stream block until they finish, so a host that decides mid-call
// that it no longer wants the answer — a web request whose client went away, a
// job that was cancelled, a shutdown in progress — previously had exactly one
// lever: llmux_close, which destroys the gateway. llmux_cancel cancels the
// in-flight work and installs a fresh context, so the handle is immediately
// usable again.
type instance struct {
	gw *gateway.Gateway

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
}

// callCtx returns the context in force right now. Reading it under the lock is
// what makes a concurrent llmux_cancel safe: the swap replaces the field, and a
// call that already took the old context simply gets cancelled.
func (i *instance) callCtx() context.Context {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ctx
}

// cancelInflight aborts every call currently running on this handle and arms a
// fresh context for the next one. Calling it with nothing in flight is a no-op
// from the host's point of view.
func (i *instance) cancelInflight() {
	i.mu.Lock()
	old := i.cancel
	i.ctx, i.cancel = context.WithCancel(context.Background())
	i.mu.Unlock()
	old()
}

// shutdown cancels whatever is running and releases the gateway's resources.
func (i *instance) shutdown() {
	i.mu.Lock()
	cancel := i.cancel
	i.mu.Unlock()
	cancel()
	_ = i.gw.Close()
}

// The registry is why handles are uint64s and not pointers. A host that calls
// with a closed or invented handle gets a clean error string; passing a stale
// uintptr back into Go would be a segfault in the host's process, and the host
// would blame llmux for a crash it could not read.
//
// Handles are never reused: the counter only increases, so a use-after-close is
// reported as such rather than silently landing on a different gateway that
// happens to have taken the slot.
var (
	regMu  sync.RWMutex
	reg    = map[uint64]*instance{}
	nextID atomic.Uint64
)

func lookup(h uint64) (*instance, error) {
	regMu.RLock()
	inst, ok := reg[h]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("llmux: unknown handle %d (never created, or already closed)", h)
	}
	return inst, nil
}

// openGateway builds a gateway from a JSON configuration document and returns
// its handle. An empty document means "defaults plus environment", the same
// thing `llmux serve` with no config file does.
//
// It does NOT call Start: no price-catalog sync, no spend flusher, no Redis
// ping, no goroutines. A shared library loaded into someone else's process must
// not start background traffic they did not ask for. Hosts that want the
// background work run the sidecar, which is what the HTTP server is for.
func openGateway(configJSON string) (h uint64, err error) {
	// A panic here (a malformed provider block reaching a nil dereference deep in
	// construction, say) must be an error, not a dead host process. See recovered.
	defer func() {
		if rec := recover(); rec != nil {
			h, err = 0, recovered(rec)
		}
	}()
	cfg, err := config.FromJSON([]byte(configJSON))
	if err != nil {
		return 0, err
	}
	gw, err := newGateway(cfg)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{gw: gw, ctx: ctx, cancel: cancel}

	h = nextID.Add(1)
	regMu.Lock()
	reg[h] = inst
	regMu.Unlock()
	return h, nil
}

// closeGateway drops the handle, cancels the instance context (which aborts any
// stream still running on it) and releases the gateway's resources. Closing an
// unknown or already-closed handle is a no-op, so a host's cleanup path is
// idempotent and a double free is not a crash.
func closeGateway(h uint64) {
	// llmux_close is void: there is no error channel to report a panic through,
	// so the backstop swallows it. The handle has already been removed from the
	// registry by the time anything that can panic runs, so a host's cleanup path
	// still makes progress instead of taking the process down. See recovered.
	defer func() { _ = recover() }()
	regMu.Lock()
	inst, ok := reg[h]
	delete(reg, h)
	regMu.Unlock()
	if !ok {
		return
	}
	inst.shutdown()
}

// cancelGateway aborts the calls in flight on h and leaves the handle open and
// usable. An unknown handle is a no-op: a host racing its own cleanup against
// its own cancel must not be punished for the order they landed in.
func cancelGateway(h uint64) {
	defer func() { _ = recover() }()
	inst, err := lookup(h)
	if err != nil {
		return
	}
	inst.cancelInflight()
}

// liveHandles reports how many handles are open. Tests use it to prove close
// actually removes the entry rather than merely returning.
func liveHandles() int {
	regMu.RLock()
	defer regMu.RUnlock()
	return len(reg)
}

// ---------------------------------------------------------------------------
// Unary call
// ---------------------------------------------------------------------------

// Methods is the closed set llmux_call accepts. It is a set of short strings
// rather than one C function per operation so the header stays stable as the
// API grows.
var Methods = []string{"chat", "embed", "models"}

// StreamMethods is the closed set llmux_stream accepts.
var StreamMethods = []string{"chat"}

func unknownMethod(method string, allowed []string) error {
	return fmt.Errorf("llmux: unknown method %q (want one of: %s)", method, strings.Join(allowed, ", "))
}

// callMethod is llmux_call. Request and response are the same OpenAI-shaped
// JSON documents the HTTP API serves, so the wire contract is reused rather
// than reinvented — a request body that works against POST /v1/chat/completions
// works here unchanged.
func callMethod(h uint64, method, requestJSON string) (out string, err error) {
	// See recovered: a panic below this line would otherwise kill the host.
	defer func() {
		if rec := recover(); rec != nil {
			out, err = "", recovered(rec)
		}
	}()
	inst, err := lookup(h)
	if err != nil {
		return "", err
	}
	switch method {
	case "chat":
		return chatCall(inst, requestJSON)
	case "embed":
		return embedCall(inst, requestJSON)
	case "models":
		out, err := json.Marshal(inst.gw.ModelList())
		if err != nil {
			return "", err
		}
		return string(out), nil
	default:
		return "", unknownMethod(method, Methods)
	}
}

func chatCall(inst *instance, requestJSON string) (string, error) {
	raw := []byte(requestJSON)
	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("llmux: invalid chat request JSON: %w", err)
	}
	if req.Stream {
		// Silently serving this unary would drop the streaming the caller asked
		// for and hand back one blob after the fact, which looks like a working
		// stream until someone measures time-to-first-token.
		return "", errors.New(`llmux: "stream": true is not valid for llmux_call; use llmux_stream`)
	}
	// ChatRaw, not Chat: the caller's ORIGINAL bytes reach the provider, so
	// fields llmux does not model survive the hop exactly as they do over HTTP.
	res, err := inst.gw.ChatRaw(inst.callCtx(), &req, raw)
	if err != nil {
		return "", err
	}
	// res.Body was serialized once on the way through; the HTTP shell writes
	// these same bytes verbatim.
	return string(res.Body), nil
}

func embedCall(inst *instance, requestJSON string) (string, error) {
	raw := []byte(requestJSON)
	var req openai.EmbeddingRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("llmux: invalid embeddings request JSON: %w", err)
	}
	resp, err := inst.gw.EmbedRaw(inst.callCtx(), &req, raw)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// errChunkAborted is returned by the yield function when the host's callback
// asked to stop. It is unwrapped at the boundary: a host that aborted its own
// stream knows it did, and should not be handed an error string for it.
var errChunkAborted = errors.New("llmux: stream aborted by the host callback")

// abortedError reports whether err is (or wraps) a host-requested abort.
func abortedError(err error) bool { return errors.Is(err, errChunkAborted) }

// errCallbackStop is what the cgo shim's emit returns when the host's callback
// returned non-zero. It is a distinct sentinel from errChunkAborted so the
// translation happens in exactly one place (emitSink.Chunk).
var errCallbackStop = errors.New("llmux: host callback returned non-zero")

// errNoCallback is llmux_stream's refusal of a NULL callback — streaming into
// nowhere is a programming error, not an empty stream.
var errNoCallback = errors.New("llmux: llmux_stream requires a non-NULL chunk callback")

// emitSink adapts the host's chunk callback to gateway.StreamSink.
//
// The sink form is used rather than the simpler Gateway.ChatStream because
// ChatStream re-marshals the typed request, which would drop any field llmux
// does not model — the same passthrough fidelity the unary path gets from
// ChatRaw. Open/Failed/Done have nothing to do here: the host learns the stream
// ended from llmux_stream returning, and learns why from its return code.
type emitSink struct{ emit func(string) error }

func (s emitSink) Open() error { return nil }

func (s emitSink) Chunk(c *openai.ChatCompletionChunk) error {
	out, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := s.emit(string(out)); err != nil {
		return errChunkAborted
	}
	return nil
}

func (s emitSink) Failed(*openai.ErrorResponse) {}
func (s emitSink) Done()                        {}

// streamMethod is llmux_stream. emit is called once per chunk with the chunk's
// JSON — byte for byte what the HTTP API writes after each `data: ` prefix in
// its SSE stream, minus the prefix and minus the terminal [DONE] frame (the
// return of this function IS the end of stream).
//
// emit returning an error stops the stream. streamMethod then returns an error
// satisfying abortedError, which the C shim reports as a clean stop rather than
// a failure — the host asked for it and already knows.
func streamMethod(h uint64, method, requestJSON string, emit func(string) error) (err error) {
	// See recovered. This one also covers the host's own chunk callback: emit
	// runs inside this call frame, so a binding whose callback trampoline panics
	// (a Python binding that failed to reacquire the GIL, a JVM binding on an
	// unattached thread) surfaces as an error rather than as a dead process.
	defer func() {
		if rec := recover(); rec != nil {
			err = recovered(rec)
		}
	}()
	inst, err := lookup(h)
	if err != nil {
		return err
	}
	if method != "chat" {
		return unknownMethod(method, StreamMethods)
	}
	raw := []byte(requestJSON)
	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("llmux: invalid chat request JSON: %w", err)
	}
	req.Stream = true

	// One context for the whole call: taking it twice could straddle an
	// llmux_cancel and stream on a context the host already abandoned.
	cctx := inst.callCtx()
	res, err := inst.gw.Prepare(cctx, req.Model)
	if err != nil {
		return err
	}
	return inst.gw.ChatStreamSink(cctx, &req, raw, res, emitSink{emit: emit})
}
