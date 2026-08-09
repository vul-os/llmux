package gateway

import (
	"context"

	"github.com/vul-os/llmux/core/keys"
)

// The request context carries the three facts every dispatch path needs about
// the caller: the authenticated virtual key, the resolved account id, and
// whether the request is being served on the account's OWN provider key (BYOK,
// and therefore unmetered).
//
// The HTTP shell (core/server) populates the context in its auth middleware and
// an embedding host populates it via Gateway.Authorize, so both entry points
// converge on the same values. The exported With*/From helpers exist so the
// shell — a different package — can do that.

type ctxKey int

const (
	keyCtxKey ctxKey = iota
	accountCtxKey
	byokCtxKey
)

// withKey attaches an authenticated virtual key to the context.
func withKey(ctx context.Context, k *keys.Key) context.Context {
	return context.WithValue(ctx, keyCtxKey, k)
}

// withAccount attaches the resolved Vulos account id to the context, so usage
// can be attributed to the account (not just the key name).
func withAccount(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, accountCtxKey, id)
}

// accountFrom returns the resolved account id from context, or "".
func accountFrom(ctx context.Context) string {
	id, _ := ctx.Value(accountCtxKey).(string)
	return id
}

// withBYOK marks the request as served via the account's own provider key
// (BYOK), so it is recorded as unmetered and not billed to the control plane.
func withBYOK(ctx context.Context, byok bool) context.Context {
	return context.WithValue(ctx, byokCtxKey, byok)
}

// byokFrom reports whether the request was served via BYOK (unmetered).
func byokFrom(ctx context.Context) bool {
	b, _ := ctx.Value(byokCtxKey).(bool)
	return b
}

// keyFrom returns the authenticated key from context, or nil.
func keyFrom(ctx context.Context) *keys.Key {
	k, _ := ctx.Value(keyCtxKey).(*keys.Key)
	return k
}

// keyName returns the authenticated key's label, or "" if unauthenticated.
func keyName(ctx context.Context) string {
	if k := keyFrom(ctx); k != nil {
		return k.Name
	}
	return ""
}

// WithKey attaches an authenticated virtual key to ctx.
func WithKey(ctx context.Context, k *keys.Key) context.Context { return withKey(ctx, k) }

// WithAccount attaches the resolved account id to ctx.
func WithAccount(ctx context.Context, id string) context.Context { return withAccount(ctx, id) }

// WithBYOK marks ctx as served on the account's own provider key (unmetered).
func WithBYOK(ctx context.Context, byok bool) context.Context { return withBYOK(ctx, byok) }

// KeyFrom returns the authenticated key in ctx, or nil.
func KeyFrom(ctx context.Context) *keys.Key { return keyFrom(ctx) }

// AccountFrom returns the resolved account id in ctx, or "".
func AccountFrom(ctx context.Context) string { return accountFrom(ctx) }

// BYOKFrom reports whether ctx was marked BYOK (unmetered).
func BYOKFrom(ctx context.Context) bool { return byokFrom(ctx) }

// KeyName returns the authenticated key's label in ctx, or "".
func KeyName(ctx context.Context) string { return keyName(ctx) }
