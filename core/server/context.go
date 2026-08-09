package server

import (
	"context"

	"github.com/vul-os/llmux/core/keys"
)

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
