package main_test

// The ABI version is compiled into the shared library, so it cannot be read
// from VERSION at runtime and has to be duplicated in ffi/. ffi/ pins that
// duplication itself — but ffi/ is a SEPARATE Go module, so `go test ./...`
// here never runs its tests, and `make test-ffi` is the only thing that would.
//
// That gap shipped: v0.1.3 was tagged and pushed with VERSION at 0.1.3 and
// ffi/abi.go still at 0.1.2, so llmux_abi_version() answered 0.1.2 from a 0.1.3
// build — breaking the stale-library detection that symbol exists to provide,
// in the direction that reports "old" for a current library. openrate shipped
// the identical defect in the same release, from the same cause.
//
// This test lives in the ROOT module deliberately, and reads ffi/'s source as
// text rather than importing it, because importing across the module boundary
// is exactly what is not possible.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFFIABIVersionTracksTheVERSIONFile(t *testing.T) {
	raw, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	version := strings.TrimSpace(string(raw))
	if version == "" {
		t.Fatal("VERSION is empty, so this test would compare nothing against nothing")
	}

	src, err := os.ReadFile("ffi/abi.go")
	if err != nil {
		t.Fatalf("read ffi/abi.go: %v", err)
	}
	m := regexp.MustCompile(`(?m)^const Version = "([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("ffi/abi.go: could not find `const Version = \"...\"`. If it moved or was " +
			"renamed this check is now examining nothing — fix the pattern, do not delete the test.")
	}
	if got := string(m[1]); got != version {
		t.Errorf("ffi/abi.go declares Version = %q but VERSION says %q.\n"+
			"A release that bumps one and not the other ships a library that misreports "+
			"itself to every host that calls llmux_abi_version().", got, version)
	}
}
