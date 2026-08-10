package main

// llmux_new and the host's environment.
//
// ffi/include/llmux.h promises the gateway is INERT: "creating it starts no
// goroutines and — unless your configuration names a Postgres DSN, which
// connects and migrates eagerly — opens no sockets."
//
// In library mode the environment is not llmux's to read. A Rails or Django app
// with DATABASE_URL exported loads libllmux, calls llmux_new with a document
// that says nothing about a database, and used to get a connection to that
// app's production database plus CREATE SCHEMA / CREATE TABLE — because
// config.FromJSON applied env AFTER the caller's document, and applyEnv took
// DATABASE_URL unconditionally.
//
// The config-level rules are tested in core/config/library_env_test.go. This
// tests the consequence at the boundary that made it dangerous: whether a
// socket is opened.

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// deadListener accepts connections and counts them, answering nothing. A
// Postgres client that dials it hangs on the startup handshake — which is also
// what makes it a good stand-in for "the host's real database": nothing is
// created, but the attempt is unmissable.
type deadListener struct {
	ln    net.Listener
	conns atomic.Int64
}

func newDeadListener(t *testing.T) *deadListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &deadListener{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.conns.Add(1)
			// Hold it open and say nothing.
			go func() { <-time.After(30 * time.Second); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return d
}

func (d *deadListener) dsn() string {
	return fmt.Sprintf("postgres://llmux:pw@%s/hostapp?sslmode=disable", d.ln.Addr().String())
}

func TestNewDoesNotAdoptTheHostsDatabaseURL(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "VULOS_DATABASE_URL"} {
		t.Run(name, func(t *testing.T) {
			db := newDeadListener(t)
			t.Setenv(name, db.dsn())

			done := make(chan struct{})
			var h uint64
			var err error
			go func() {
				defer close(done)
				h, err = openGateway(`{"server":{"addr":":4000"}}`)
			}()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("llmux_new is still running 20s in — it dialled the host's database and " +
					"is blocked on the Postgres handshake, which is exactly the hang this must not do")
			}
			if err != nil {
				t.Fatalf("openGateway: %v — a host's DATABASE_URL must not be able to make "+
					"llmux_new fail either", err)
			}
			closeGateway(h)

			if n := db.conns.Load(); n != 0 {
				t.Errorf("llmux_new opened %d connection(s) to the address in %s.\n"+
					"  That is the host application's database. Connecting to it means running "+
					"CREATE SCHEMA / CREATE TABLE in someone else's production data, from a "+
					"shared library they merely loaded — and llmux.h promises inertness unless "+
					"THEIR configuration names a DSN.", n, name)
			}
		})
	}
}

// The other half of the promise, so the test above cannot pass by llmux having
// stopped talking to Postgres altogether: a document that DOES name a DSN still
// connects eagerly, exactly as the header says.
func TestNewStillConnectsWhenTheDocumentNamesADSN(t *testing.T) {
	db := newDeadListener(t)
	// A 1s connect bound so this runs synchronously and leaves no goroutine
	// behind: the listener never completes the handshake, so the call fails —
	// what is asserted is the DIAL, not the outcome.
	doc := fmt.Sprintf(`{"server":{"addr":":4000"},"postgres":%q,"postgres_connect_timeout_seconds":1}`,
		db.dsn())

	start := time.Now()
	h, err := openGateway(doc)
	if err == nil {
		closeGateway(h)
		t.Fatal("openGateway succeeded against a listener that never answers")
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Errorf("took %s for a 1s connect bound", took)
	}
	if db.conns.Load() == 0 {
		t.Fatal("openGateway never dialled the DSN the document named — the eager-connect " +
			"behaviour llmux.h documents is gone, and TestNewDoesNotAdoptTheHostsDatabaseURL " +
			"is then asserting nothing")
	}
}
