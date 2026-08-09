//go:build noui

// Package webui is the admin console. Under the `noui` build tag the console
// is NOT compiled in: HTML and Licenses return nil, Enabled reports false, and
// core/server mounts a small JSON stub at /ui saying so. This exists for
// size-sensitive builds (and for embedders who never want the console in their
// binary at all); both tag states are compiled in CI.
package webui

// HTML returns nil: the admin console was not built into this binary.
func HTML() []byte { return nil }

// Licenses returns nil: the notices ride with the console, which is absent.
func Licenses() []byte { return nil }

// Enabled reports whether the admin console is compiled into this binary.
func Enabled() bool { return false }
