package main

// Closing a handle while a call is running.
//
// llmux_close used to cancel the instance context and then call
// Gateway.Close immediately, without waiting. Gateway.Close shuts the Redis
// client and the Postgres pool; a call still in flight is then using a closed
// pool, inside the host's process, where the failure mode is whatever go-redis
// or pgx does to a client that vanished under it.
//
// Close now drains: cancel, wait for the in-flight calls to return (bounded —
// see closeDrainGrace), then release. And a call that arrives after close began
// is refused rather than admitted into a gateway that is being torn down.

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCloseWaitsForACallInFlight uses a stream that is parked in its callback:
// as long as the callback has not returned, the call has not returned, so
// llmux_close must not have returned either.
func TestCloseWaitsForACallInFlight(t *testing.T) {
	url := silentStreamUpstream(t, 4)
	h, err := openGateway(streamConfig(url, -1, -1))
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}

	inCallback := make(chan struct{})
	release := make(chan struct{})
	streamDone := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(streamDone)
		_ = streamMethod(h, "chat", chatReq, func(string) error {
			once.Do(func() {
				close(inCallback)
				<-release // park here, holding the call open
			})
			return nil
		})
	}()

	select {
	case <-inCallback:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never reached the callback; there was nothing in flight to drain")
	}

	closeReturned := make(chan struct{})
	go func() {
		closeGateway(h)
		close(closeReturned)
	}()

	// While the call is parked, close must still be waiting.
	select {
	case <-closeReturned:
		t.Fatal("llmux_close returned while a call was still running. Gateway.Close shuts the " +
			"Redis client and the Postgres pool, so it just pulled them out from under an " +
			"in-flight call inside the host's process.")
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	select {
	case <-closeReturned:
	case <-time.After(10 * time.Second):
		t.Fatal("llmux_close never returned after the call finished")
	}
	<-streamDone
	if liveHandles() != 0 && func() bool { _, e := lookup(h); return e == nil }() {
		t.Error("the handle survived close")
	}
}

// The drain is bounded, so a host cannot lose a thread to a call that ignores
// cancellation — and, in particular, calling llmux_close from INSIDE a chunk
// callback stalls for the grace period instead of deadlocking forever.
func TestCloseIsBoundedWhenACallWillNotStop(t *testing.T) {
	if closeDrainGrace > 30*time.Second {
		t.Fatalf("closeDrainGrace is %s; a void C function blocking that long is its own defect",
			closeDrainGrace)
	}
	url := silentStreamUpstream(t, 4)
	h, err := openGateway(streamConfig(url, -1, -1))
	if err != nil {
		t.Fatalf("openGateway: %v", err)
	}

	inCallback := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var once sync.Once
	go func() {
		_ = streamMethod(h, "chat", chatReq, func(string) error {
			once.Do(func() {
				close(inCallback)
				<-release
			})
			return nil
		})
	}()
	select {
	case <-inCallback:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never reached the callback")
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		closeGateway(h)
		done <- time.Since(start)
	}()
	select {
	case took := <-done:
		if took < closeDrainGrace {
			t.Errorf("close returned after %s, before the %s grace period — it did not actually "+
				"wait, so TestCloseWaitsForACallInFlight is the only thing holding the drain",
				took, closeDrainGrace)
		}
	case <-time.After(closeDrainGrace + 15*time.Second):
		t.Fatalf("llmux_close did not return within %s of the grace period. Unbounded waiting in "+
			"a void C function means a host that calls close from inside its own callback "+
			"deadlocks with no way out.", closeDrainGrace)
	}
}

// A call that arrives while close is draining must be refused, not admitted
// into a gateway whose resources are about to be released.
func TestACallStartedDuringCloseIsRefused(t *testing.T) {
	h, _ := newTestHandle(t, "hello")
	inst, err := lookup(h)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	inst.mu.Lock()
	inst.closing = true
	inst.mu.Unlock()

	if _, _, err := inst.begin(); err == nil {
		t.Fatal("a call was admitted to a handle that is closing")
	} else if !strings.Contains(err.Error(), "being closed") {
		t.Errorf("error = %v, want a being-closed diagnosis", err)
	}
	if _, err := callMethod(h, "models", ""); err == nil {
		t.Error("callMethod ran against a closing handle")
	}
}
