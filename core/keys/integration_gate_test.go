package keys

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

// The Postgres/Redis tests in this package are the ONLY coverage of the
// cross-replica properties — spend that survives a restart, budgets that fail
// closed on a store error, tokens hashed at rest, a rate limit that holds
// across replicas. They skip when the env is unset, and a SILENT skip is
// exactly how a suite comes to "pass" while verifying nothing.
//
// So the gate records every skip, TestMain prints a loud banner naming what was
// not verified, and the size of the gated corpus is checked against the source:
// add a gated test and forget to wire it to the gate, and the count no longer
// matches.

const (
	pgEnv    = "LLMUX_TEST_POSTGRES"
	redisEnv = "LLMUX_TEST_REDIS"
	// requireEnv makes a skip a FAILURE. CI sets it, because there the service
	// containers are guaranteed: a skip means they did not come up, and a green
	// run with zero integration coverage is worse than a red one.
	requireEnv = "LLMUX_REQUIRE_INTEGRATION"
)

// unverifiedProperties names, in plain words, what nobody checked when the
// gated tests skip. A count of skipped tests is not information; this is.
var unverifiedProperties = []string{
	"virtual-key spend persists across process restarts (Postgres store)",
	"bearer tokens are stored only as sha256 (no plaintext credentials at rest)",
	"llmux's tables live under the dedicated schema when sharing a database",
	"pending spend is recovered after a database error (no silently free requests)",
	"Redis rate-limit keys are hashed, and the fixed window holds across replicas",
	"a Redis outage degrades the RPM cap instead of removing it",
}

var (
	gateMu      sync.Mutex
	gateRan     []string
	gateSkipped []string
)

// gateRecord notes whether a gated test actually ran. Call it BEFORE t.Skip,
// which does not return.
func gateRecord(name string, ran bool) {
	gateMu.Lock()
	defer gateMu.Unlock()
	if ran {
		gateRan = append(gateRan, name)
		return
	}
	gateSkipped = append(gateSkipped, name)
}

// gatedCallSites counts calls to the integration gates across this package's
// test sources, so the corpus size comes from the code rather than a number
// somebody has to remember to bump.
func gatedCallSites() (int, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return 0, err
	}
	gates := map[string]bool{"testDSN": true, "testRedisClient": true}
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
			id, ok := call.Fun.(*ast.Ident)
			if !ok || !gates[id.Name] {
				return true
			}
			// Skip the declarations themselves (they are FuncDecls, not calls).
			n++
			return true
		})
	}
	return n, nil
}

// minGatedTests is a floor so a broken scan cannot report "0 gated tests, all
// accounted for".
const minGatedTests = 6

func TestMain(m *testing.M) {
	code := m.Run()

	// A filtered run (`go test -run X`) legitimately executes a subset, so the
	// corpus check only applies to a full run.
	filtered := false
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		filtered = true
	}

	declared, err := gatedCallSites()
	if err != nil {
		fmt.Fprintf(os.Stderr, "core/keys: could not count gated tests: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "core/keys: found only %d gated call sites (floor %d) — the integration "+
				"corpus scan is broken\n", declared, minGatedTests)
			os.Exit(1)
		}
		if observed != declared {
			fmt.Fprintf(os.Stderr, "core/keys: %d gated integration call sites exist in the source but only %d "+
				"reached the gate — a gated test is bypassing testDSN/testRedisClient and its result is not "+
				"being accounted for\n", declared, observed)
			os.Exit(1)
		}
	}

	// In CI the service containers are always present, so a skip there means the
	// services failed to come up and the run has ZERO integration coverage while
	// still reporting green. requireEnv turns that into a failure.
	if len(skipped) > 0 && os.Getenv(requireEnv) != "" {
		fmt.Fprintf(os.Stderr, "core/keys: %s is set but %d integration test(s) SKIPPED (%s) — the run has no "+
			"cross-replica coverage; check the Postgres/Redis services\n",
			requireEnv, len(skipped), strings.Join(skipped, ", "))
		code = 1
	}

	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, `
========================= NOT VERIFIED (core/keys) =========================
%d of %d integration test(s) were SKIPPED: %s
Reason: %s / %s not set (or the service was unreachable).

These properties were NOT checked by this run:
  - %s

Run them with:
  export %s='postgres://user@localhost:5432/llmux_test?sslmode=disable'
  export %s='localhost:6379'
  go test ./core/keys/ ./core/cache/
============================================================================
`, len(skipped), observed, strings.Join(skipped, ", "), pgEnv, redisEnv,
			strings.Join(unverifiedProperties, "\n  - "), pgEnv, redisEnv)
	}

	os.Exit(code)
}
