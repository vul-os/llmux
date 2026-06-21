package server

import (
	"context"

	"github.com/llmux/llmux/core/keys"
)

type ctxKey int

const keyCtxKey ctxKey = iota

// withKey attaches an authenticated virtual key to the context.
func withKey(ctx context.Context, k *keys.Key) context.Context {
	return context.WithValue(ctx, keyCtxKey, k)
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
