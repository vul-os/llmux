// Package embedwall exists to be UNREACHABLE.
//
// It is the canary for llmux's module wall. The embeddability suite
// (../embedtest, module github.com/vul-os/llmux-embedtest) claims that if its
// tests compile, they used nothing but llmux's exported API. That claim rests
// entirely on Go's internal-package rule — and the rule is about IMPORT PATH
// PREFIXES, not module boundaries:
//
//	an import of a path containing "internal" is allowed only from code whose
//	import path begins with the prefix up to that "internal" element.
//
// Here the prefix is github.com/vul-os/llmux. A test module named
// github.com/vul-os/llmux/embedtest sits UNDER that prefix, so it could import
// this package freely and the whole guard suite would be a false green — the
// mistake that shipped in a sibling repo and was caught only by mutation. A
// module named github.com/vul-os/llmux-embedtest does not, so the import is
// refused by the compiler.
//
// embedtest's TestInternalWallRefusesAnInternalImport builds a probe package
// that blank-imports this one and REQUIRES the build to fail. That is the only
// thing standing between "our public API is sufficient" and a sentence in a
// comment. Do not delete this package, do not export anything through it, and
// do not import it from anywhere: it is load-bearing precisely by being
// off-limits.
package embedwall

// Marker is referenced by the probe so the import is not elided as unused in
// any future variant of the probe that wants a real reference rather than a
// blank one.
const Marker = "llmux internal wall canary"
