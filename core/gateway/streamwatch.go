package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrStreamTimeout is returned when a streaming call was ended by llmux's own
// liveness bound rather than by the upstream, the client, or a transport error.
// Callers can tell the three apart with errors.Is.
var ErrStreamTimeout = errors.New("llmux: streaming upstream stopped responding")

// streamWatchdog bounds a streaming call by LIVENESS instead of by total time.
//
// A wall-clock deadline is the wrong instrument for a stream: a legitimate long
// generation and a dead connection look identical to it, so any value large
// enough not to truncate real work is also large enough to be useless. What
// separates the two is whether anything is still arriving. So there are two
// clocks and they are the same timer:
//
//   - firstByte, armed when the call starts, is how long the upstream may take
//     to produce its first chunk (queueing, a cold model load, a slow prefill).
//   - idle, re-armed on every chunk, is the longest permitted gap between
//     chunks once tokens are flowing. A stream that keeps producing never
//     expires, however long it runs in total.
//
// On expiry the watchdog cancels the context the provider call is using, which
// unblocks the blocked body read. The provider then returns a context error, so
// expired() is what distinguishes "llmux gave up" from "the caller cancelled" —
// both arrive as context.Canceled otherwise.
//
// Either bound can be turned off (config: a negative value). With both off the
// watchdog installs no timer at all and behaves exactly as the unbounded code
// it replaced.
type streamWatchdog struct {
	ctx    context.Context
	cancel context.CancelFunc
	idle   time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	fired   bool
	phase   string // which bound was armed when it fired, for the message
	stopped bool
}

// newStreamWatchdog derives a cancellable context from parent and arms the
// first-byte bound. The caller MUST call stop (defer it) to release the timer.
func newStreamWatchdog(parent context.Context, firstByte, idle time.Duration) *streamWatchdog {
	ctx, cancel := context.WithCancel(parent)
	w := &streamWatchdog{ctx: ctx, cancel: cancel, idle: idle, phase: "first chunk"}
	w.arm(firstByte, "first chunk")
	return w
}

// arm (re)sets the timer to d. d <= 0 disarms: that bound is switched off.
func (w *streamWatchdog) arm(d time.Duration, phase string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.fired {
		return
	}
	w.phase = phase
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if d <= 0 {
		return
	}
	w.timer = time.AfterFunc(d, w.expire)
}

// expire runs on the timer's own goroutine. It does nothing but flip a flag and
// cancel — a panic here could not be recovered by the caller's frame, so there
// is deliberately nothing here that can panic.
func (w *streamWatchdog) expire() {
	w.mu.Lock()
	if w.stopped || w.fired {
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

// beat records that a chunk arrived and restarts the clock on the idle bound.
func (w *streamWatchdog) beat() { w.arm(w.idle, "chunk") }

// restartFirstByte re-arms the first-byte bound. Failover to the next target is
// a fresh attempt against a different upstream, so it gets a fresh budget
// rather than inheriting what the previous target already burned.
func (w *streamWatchdog) restartFirstByte(d time.Duration) { w.arm(d, "first chunk") }

// expired reports whether this watchdog, rather than anything else, ended the call.
func (w *streamWatchdog) expired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// stop releases the timer and the context. Safe to call more than once.
func (w *streamWatchdog) stop() {
	w.mu.Lock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
	w.cancel()
}

// err builds the error to report in place of the context cancellation the
// provider saw, naming which bound expired and how long it was.
func (w *streamWatchdog) err(firstByte, idle time.Duration) error {
	w.mu.Lock()
	phase := w.phase
	w.mu.Unlock()
	if phase == "chunk" {
		return fmt.Errorf("%w: no chunk for %s (stream_idle_timeout_seconds)", ErrStreamTimeout, idle)
	}
	return fmt.Errorf("%w: no first chunk within %s (stream_first_byte_timeout_seconds)",
		ErrStreamTimeout, firstByte)
}
