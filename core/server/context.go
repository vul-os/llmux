package server

import (
	"context"

	"github.com/llmux/llmux/core/keys"
)

type ctxKey int

const (
	keyCtxKey ctxKey = iota
	accountCtxKey
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
