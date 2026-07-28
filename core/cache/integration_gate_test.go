package cache

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The Redis tests here are the only coverage of the SHARED cache path — the one
// that matters across replicas. They skip when LLMUX_TEST_REDIS is unset, and a
// silent skip is how a suite comes to "pass" while verifying nothing. So every
// skip is recorded, the run says out loud what it did not verify, and the size
// of the gated corpus is checked against the source. (Mirrors core/keys.)

const (
	redisEnv = "LLMUX_TEST_REDIS"
	// requireEnv makes a skip a FAILURE; CI sets it, where the Redis service is
	// guaranteed and a skip means it did not come up.
	requireEnv = "LLMUX_REQUIRE_INTEGRATION"
)

// unverifiedProperties says, in plain words, what nobody checked on a skip.
var unverifiedProperties = []string{
	"cached responses round-trip through a real Redis (shared across replicas)",
	"cache entries expire on the configured TTL in Redis, not just in the LRU",
}

var (
	gateMu      sync.Mutex
	gateRan     []string
	gateSkipped []string
)

// gateRecord notes whether a gated test ran. Call it BEFORE t.Skip.
func gateRecord(name string, ran bool) {
	gateMu.Lock()
	defer gateMu.Unlock()
	if ran {
		gateRan = append(gateRan, name)
		return
	}
	gateSkipped = append(gateSkipped, name)
}

// gatedCallSites counts calls to the Redis gate across this package's tests, so
// the corpus size comes from the code rather than a remembered number.
func gatedCallSites() (int, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return 0, err
	}
	fset := token.NewFileSet()
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			return 0, err
		}
		ast.Inspect(f, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "testRedis" {
				n++
			}
			return true
		})
	}
	return n, nil
}

// minGatedTests is a floor so a broken scan cannot report "0 gated tests, all
// accounted for".
const minGatedTests = 1

func TestMain(m *testing.M) {
	code := m.Run()

	filtered := false
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		filtered = true
	}

	declared, err := gatedCallSites()
	if err != nil {
		fmt.Fprintf(os.Stderr, "core/cache: could not count gated tests: %v\n", err)
		os.Exit(1)
	}

	gateMu.Lock()
	ran, skipped := append([]string(nil), gateRan...), append([]string(nil), gateSkipped...)
	gateMu.Unlock()
	sort.Strings(ran)
	sort.Strings(skipped)
	observed := len(ran) + len(skipped)

	if !filtered {
		if declared < minGatedTests {
			fmt.Fprintf(os.Stderr, "core/cache: found only %d gated call sites (floor %d) — the integration "+
				"corpus scan is broken\n", declared, minGatedTests)
			os.Exit(1)
		}
		if observed != declared {
			fmt.Fprintf(os.Stderr, "core/cache: %d gated call sites exist in the source but only %d reached the "+
				"gate — a gated test is bypassing testRedis and its result is not accounted for\n",
				declared, observed)
			os.Exit(1)
		}
	}

	if len(skipped) > 0 && os.Getenv(requireEnv) != "" {
		fmt.Fprintf(os.Stderr, "core/cache: %s is set but %d integration test(s) SKIPPED (%s) — the run has no "+
			"shared-cache coverage; check the Redis service\n",
			requireEnv, len(skipped), strings.Join(skipped, ", "))
		code = 1
	}

	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, `
======================== NOT VERIFIED (core/cache) =========================
%d of %d integration test(s) were SKIPPED: %s
Reason: %s not set (or Redis was unreachable).

These properties were NOT checked by this run:
  - %s

Run them with:
  export %s='localhost:6379'
  go test ./core/cache/
============================================================================
`, len(skipped), observed, strings.Join(skipped, ", "), redisEnv,
			strings.Join(unverifiedProperties, "\n  - "), redisEnv)
	}

	os.Exit(code)
}
