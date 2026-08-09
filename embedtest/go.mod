// embedtest is a SEPARATE Go module on purpose, and its path is deliberately
// OUTSIDE github.com/vul-os/llmux/... — see doc.go and internalprobe/.
module github.com/vul-os/llmux-embedtest

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

// Until the module is published, the public import path resolves to the tree
// above. This does NOT weaken the guard: Go forbids importing another module's
// internal/ packages even through a replace, so embedtest can only reach llmux
// through its exported API — which is exactly what these tests assert.
replace github.com/vul-os/llmux => ../
