package server

import (
	"context"

	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/keys"
)

// The request-context accessors live in core/gateway, because the gateway's
// dispatch paths read them and an embedding host (which never touches this
// package) writes them via Gateway.Authorize. The HTTP shell's auth middleware
// writes the same values, so both entry points converge. These are thin
// aliases so the handlers read the same as they always did.

// withKey attaches an authenticated virtual key to the context.
func withKey(ctx context.Context, k *keys.Key) context.Context { return gateway.WithKey(ctx, k) }

// withAccount attaches the resolved Vulos account id to the context.
func withAccount(ctx context.Context, id string) context.Context {
	return gateway.WithAccount(ctx, id)
}

// withBYOK marks the request as served via the account's own provider key.
func withBYOK(ctx context.Context, byok bool) context.Context { return gateway.WithBYOK(ctx, byok) }

// keyFrom returns the authenticated key from context, or nil.
func keyFrom(ctx context.Context) *keys.Key { return gateway.KeyFrom(ctx) }
