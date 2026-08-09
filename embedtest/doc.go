// Package embedtest is the embeddability guard suite: it proves llmux is a
// library, from OUTSIDE llmux.
//
// It is a separate Go module (see go.mod) rather than a package in the main
// module, and that is the whole point of it:
//
//   - Go forbids importing another MODULE's internal/ packages, even through a
//     `replace`. So if any of these tests compile, the public API was
//     sufficient — no internal escape hatch was used to make them pass. A test
//     living in the main module could quietly reach into an unexported corner
//     and prove nothing about embeddability.
//   - `go test ./...` at the repo root does not descend into a nested module,
//     so these tests need their own CI step. That step runs through
//     scripts/go-test-gate.sh, which fails when zero tests ran — because
//     `go test` on a module whose tests all vanished exits 0.
//
// What is guarded here, in the order the guards were written:
//
//	G1  library_test.go     — the public API alone can build and drive a Gateway.
//	G2  cold_test.go        — construction is inert: no goroutines, no round
//	                          trips, no adoption of provider keys the config
//	                          never named.
//	G3  direction_test.go   — core/gateway does not depend on core/server or web.
//	G5  buildstate_test.go  — both UI build states compile; -tags noui is
//	                          measurably smaller and answers with the stub.
//	G7  uibytes_test.go     — a host that imports only the library links NO UI
//	                          bytes at all.
//
// Every one of these was mutation-tested: the guarded property was broken, the
// guard was watched to fail, and the break was reverted. A guard nobody has
// seen fail is decoration.
package embedtest
