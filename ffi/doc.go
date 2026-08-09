// Command llmux-ffi is the C-ABI shared-library build of llmux: the same
// in-process gateway that core/gateway gives a Go host, exposed to every other
// language through six C functions.
//
// It is built with `go build -buildmode=c-shared`, which produces
// libllmux.{so,dylib,dll} plus a generated header. The hand-written, stable
// header this project supports is include/llmux.h; see README.md for the ABI
// and for the costs of loading a Go runtime into someone else's process.
//
// # Why this is a separate module
//
// Two reasons, both load-bearing.
//
//  1. cgo must not leak into the main module's build. `go build ./...`,
//     `go vet ./...` and `go build -tags noui ./...` at the repo root must keep
//     working with CGO_ENABLED=0, and the embeddability guards in embedtest/
//     assert properties of the pure-Go library. A nested module is invisible to
//     the root's `./...`, so nothing here can affect any of that. (A build tag
//     would not be enough: a package whose every file is excluded by tags is a
//     hard error for `go build ./...`, not a skip.)
//
//  2. The module path is github.com/vul-os/llmux-ffi, NOT
//     github.com/vul-os/llmux/ffi. Go's internal rule compares import-path
//     prefixes, so a package under the llmux/ prefix could import
//     github.com/vul-os/llmux/internal/... freely. Sitting outside the prefix
//     makes the compiler enforce that the C ABI is built on exactly the
//     exported surface a third-party embedder has — the same wall
//     embedtest/wall_test.go exists to keep standing. TestFFIUsesOnlyThePublicAPI
//     asserts this by name.
//
// # Layout
//
//   - abi.go      — the whole implementation, in pure Go. No cgo, so it is
//     unit-testable with plain `go test` and readable without
//     knowing cgo.
//   - cshared.go  — the cgo shim: //export wrappers, C string conversion, and
//     the trampoline that calls a C function pointer. Nothing
//     but marshalling lives here.
//   - include/    — the stable hand-written header.
//   - ctest/      — the C smoke test that dlopens the built library.
//   - fakeupstream/ — a fake OpenAI-compatible server, so the C smoke test and
//     the benchmark exercise a real request path with no
//     provider account.
//   - bench/      — in-process (C ABI) vs loopback HTTP latency.
package main
