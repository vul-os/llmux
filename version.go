// Package llmux is the module's root package. It exists for one thing: to make
// the release version a value the build system computes, rather than a string
// somebody has to remember to retype.
//
// The gateway itself lives in core/gateway; nothing else belongs here.
package llmux

import (
	_ "embed"
	"strings"
)

// versionFile is the repository's VERSION file, embedded at COMPILE time.
//
// The embed has to happen in THIS directory. `//go:embed ../VERSION` is a
// compile error ("invalid pattern syntax" — embed patterns may not escape the
// package directory), and a symlink to it is rejected too ("cannot embed
// irregular file"). Both were tried; neither is a matter of taste. So the one
// package that can read VERSION at build time is the package that sits beside
// it, and everything that needs the version imports that package.
//
//go:embed VERSION
var versionFile string

// Version is the llmux release this build came from — "0.1.6", no leading v.
//
// It is DERIVED, not declared. There is no copy of the version string anywhere
// in Go source to fall out of step with a release: bumping VERSION bumps this,
// and a build with no VERSION file next to this one does not compile at all.
//
// This is what llmux_abi_version reports (see ffi/abi.go). That symbol exists so
// a host can compare the shared library it just dlopen'd against the version it
// was built for and refuse a stale .so on its load path. v0.1.3 shipped with
// VERSION at 0.1.3 and the ABI constant still reading 0.1.2, which made that
// check answer "stale" for a library that was current — the failure direction
// that costs a host its startup rather than merely its warning. A hand-typed
// constant is what made that possible, so there is no longer a hand-typed
// constant.
var Version = strings.TrimSpace(versionFile)
