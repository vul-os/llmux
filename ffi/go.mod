// ffi is a SEPARATE Go module on purpose, and — like embedtest/ — its path is
// deliberately OUTSIDE github.com/vul-os/llmux/... See doc.go for both reasons:
// it keeps cgo out of the main module's pure-Go build, and it puts the C-ABI
// layer on the far side of Go's internal/ wall, so the shared library can only
// use the same exported API any other embedder has.
module github.com/vul-os/llmux-ffi

go 1.25.0

toolchain go1.25.12

require github.com/vul-os/llmux v0.0.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/redis/go-redis/v9 v9.20.1 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)

// The shared library is always built from this checkout, against the tree it
// ships with — a .so built from ffi/ and a core/gateway from a different
// version is exactly the mismatch llmux_abi_version exists to expose.
replace github.com/vul-os/llmux => ../
