// Command bench measures what the C ABI actually buys: the latency of one chat
// request made IN PROCESS through libllmux, against the same request made over
// loopback HTTP to `llmux serve`.
//
// Both paths are driven from the same Go program, so the number is a difference
// between two mechanisms and not between two HTTP clients written in different
// languages. Both hit the same fake upstream over loopback, so the upstream's
// own cost is common to both and cancels out of the delta.
//
//	in-process : Go -> cgo -> llmux_call -> gateway -> loopback -> fake upstream
//	loopback   : Go -> net/http -> llmux HTTP server -> gateway -> loopback -> fake upstream
//
// The only difference between the two rows is the transport in the first hop.
// That is the thing being measured.
//
// Usage:
//
//	go run ./bench -lib <path to libllmux.{so,dylib}> [-n 500] [-warmup 50]
//
// The library is dlopened at runtime rather than linked, so this program tests
// the same artifact a foreign host would load.
package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>

typedef uint64_t (*new_fn)(const char*, char**);
typedef char*    (*call_fn)(uint64_t, const char*, const char*, char**);
typedef void     (*free_fn)(char*);
typedef void     (*close_fn)(uint64_t);
typedef const char* (*version_fn)(void);

static new_fn     p_new;
static call_fn    p_call;
static free_fn    p_free;
static close_fn   p_close;
static version_fn p_version;

static const char* bench_dlopen(const char* path) {
	void* h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (!h) return dlerror();
	p_new     = (new_fn)dlsym(h, "llmux_new");
	p_call    = (call_fn)dlsym(h, "llmux_call");
	p_free    = (free_fn)dlsym(h, "llmux_free");
	p_close   = (close_fn)dlsym(h, "llmux_close");
	p_version = (version_fn)dlsym(h, "llmux_abi_version");
	if (!p_new || !p_call || !p_free || !p_close || !p_version) return "missing symbol";
	return 0;
}

static const char* bench_version(void)                       { return p_version(); }
static uint64_t bench_new(const char* cfg, char** err)        { return p_new(cfg, err); }
static char* bench_call(uint64_t h, const char* m, const char* r, char** err) { return p_call(h, m, r, err); }
static void bench_free(char* p)                               { p_free(p); }
static void bench_close(uint64_t h)                           { p_close(h); }
*/
import "C"

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"time"
	"unsafe"

	"github.com/vul-os/llmux-ffi/internal/fake"
	"github.com/vul-os/llmux/core/config"
	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/server"
)

const requestJSON = `{"model":"demo","messages":[{"role":"user","content":"ping"}]}`

func main() {
	lib := flag.String("lib", "", "path to the built libllmux shared library (required)")
	n := flag.Int("n", 500, "measured requests per mechanism")
	warmup := flag.Int("warmup", 50, "unmeasured warmup requests per mechanism")
	flag.Parse()
	if *lib == "" {
		log.Fatal("bench: -lib is required (build it with scripts/build-ffi.sh)")
	}

	// One fake upstream, shared by both mechanisms.
	up := fake.New("alpha bravo charlie delta")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	go func() { _ = (&http.Server{Handler: up.Handler()}).Serve(ln) }()
	upstreamURL := "http://" + ln.Addr().String()
	cfgJSON := fake.ConfigJSON(upstreamURL)

	ffi := openLibrary(*lib, cfgJSON)
	defer C.bench_close(ffi)
	http := startLoopback(cfgJSON)
	defer http.stop()

	// Two workloads, because they answer two different questions.
	//
	// "chat" is the honest end-to-end number: it includes the upstream call,
	// which is what a real request pays and which is IDENTICAL for both
	// mechanisms. It tells you how much of a real request the transport is.
	//
	// "models" is answered from memory by the gateway with no upstream at all,
	// so it isolates the boundary itself: cgo call + JSON in + JSON out versus
	// a loopback TCP round trip + HTTP framing. That is the number that does
	// not shrink when the model gets slower.
	chatIn := measure(*n, *warmup, func() { ffiCall(ffi, "chat", requestJSON) })
	chatHTTP := measure(*n, *warmup, func() { http.post("/v1/chat/completions", requestJSON) })
	metaIn := measure(*n, *warmup, func() { ffiCall(ffi, "models", "") })
	metaHTTP := measure(*n, *warmup, func() { http.get("/v1/models") })

	fmt.Printf("\nllmux request latency — %d measured requests each, %d warmup, one host\n", *n, *warmup)
	row := func(label string, s samples) {
		fmt.Printf("  %-34s %9s %9s %9s %9s\n", label, s.min(), s.pct(50), s.pct(95), s.mean())
	}
	fmt.Printf("\n  %-34s %9s %9s %9s %9s\n", "", "min", "median", "p95", "mean")
	fmt.Println("  chat (includes the upstream round trip, identical for both):")
	row("    in-process (C ABI)", chatIn)
	row("    loopback HTTP (sidecar)", chatHTTP)
	delta("    saved by going in-process", chatHTTP, chatIn)
	fmt.Println("  models (answered from memory — this is the boundary itself):")
	row("    in-process (C ABI)", metaIn)
	row("    loopback HTTP (sidecar)", metaHTTP)
	delta("    saved by going in-process", metaHTTP, metaIn)
	fmt.Printf("\n  The models rows are the transport cost with nothing else in it. The chat\n" +
		"  rows show what fraction of a request that is when the request actually\n" +
		"  does something — and against a REAL model, which answers in hundreds of\n" +
		"  milliseconds, the same absolute saving is far below the noise floor.\n")
}

func delta(label string, slow, fast samples) {
	d := slow.pctDur(50) - fast.pctDur(50)
	pct := 100 * float64(d) / float64(slow.pctDur(50))
	fmt.Printf("  %-34s %9s  (%.0f%% of the loopback median)\n", label, round(d), pct)
}

func measure(n, warmup int, one func()) samples {
	for i := 0; i < warmup; i++ {
		one()
	}
	out := make(samples, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		one()
		out = append(out, time.Since(start))
	}
	return out
}

// ---------------------------------------------------------------------------

type samples []time.Duration

func (s samples) pctDur(p int) time.Duration {
	if len(s) == 0 {
		return 0
	}
	sorted := append(samples(nil), s...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted)-1)*p/100 + 0
	return sorted[idx]
}
func (s samples) pct(p int) string { return round(s.pctDur(p)) }
func (s samples) min() string      { return round(s.pctDur(0)) }
func (s samples) mean() string {
	if len(s) == 0 {
		return "-"
	}
	var total time.Duration
	for _, d := range s {
		total += d
	}
	return round(total / time.Duration(len(s)))
}

func round(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return d.String()
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1e3)
	default:
		return fmt.Sprintf("%.3fms", float64(d.Nanoseconds())/1e6)
	}
}

// ---------------------------------------------------------------------------

func openLibrary(libPath, cfgJSON string) C.uint64_t {
	cPath := C.CString(libPath)
	defer C.free(unsafe.Pointer(cPath))
	if e := C.bench_dlopen(cPath); e != nil {
		log.Fatalf("dlopen %s: %s", libPath, C.GoString(e))
	}
	fmt.Printf("in-process: loaded %s (llmux_abi_version = %s)\n",
		libPath, C.GoString(C.bench_version()))

	cCfg := C.CString(cfgJSON)
	defer C.free(unsafe.Pointer(cCfg))
	var cerr *C.char
	h := C.bench_new(cCfg, &cerr)
	if h == 0 {
		msg := C.GoString(cerr)
		C.bench_free(cerr)
		log.Fatalf("llmux_new: %s", msg)
	}
	return h
}

// ffiCall is one llmux_call, including the C string conversions a real host
// pays on every call. Measuring without them would flatter the ABI.
func ffiCall(h C.uint64_t, method, request string) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	cReq := C.CString(request)
	defer C.free(unsafe.Pointer(cReq))
	var err *C.char
	out := C.bench_call(h, cMethod, cReq, &err)
	if out == nil {
		msg := C.GoString(err)
		C.bench_free(err)
		log.Fatalf("llmux_call %s: %s", method, msg)
	}
	C.bench_free(out)
}

type loopback struct {
	client *http.Client
	base   string
	stop   func()
}

func startLoopback(cfgJSON string) *loopback {
	cfg, err := config.FromJSON([]byte(cfgJSON))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		log.Fatalf("gateway.New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: server.NewWithGateway(gw, server.DefaultOptions()).Handler()}
	go func() { _ = srv.Serve(ln) }()
	fmt.Printf("loopback:   llmux HTTP server on %s\n", ln.Addr())

	return &loopback{
		// Keep-alive is on, so the sidecar row is measured at its BEST: no
		// per-request TCP handshake. A benchmark that reconnected every time
		// would be measuring a client bug, not the sidecar.
		client: &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}},
		base:   "http://" + ln.Addr().String(),
		stop: func() {
			_ = srv.Close()
			_ = gw.Close()
		},
	}
}

func (l *loopback) do(req *http.Request) {
	resp, err := l.client.Do(req)
	if err != nil {
		log.Fatalf("http: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("http %d: %s", resp.StatusCode, body)
	}
}

func (l *loopback) post(path, body string) {
	req, _ := http.NewRequest(http.MethodPost, l.base+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	l.do(req)
}

func (l *loopback) get(path string) {
	req, _ := http.NewRequest(http.MethodGet, l.base+path, nil)
	l.do(req)
}
