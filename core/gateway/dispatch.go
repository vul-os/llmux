package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/router"
)

// retryableStatus reports whether an upstream status warrants a retry/fallback.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// shouldFailover reports whether err is worth trying another target for.
func shouldFailover(err error) bool {
	if pe := AsProviderError(err); pe != nil {
		return retryableStatus(pe.Status())
	}
	return true // transport-level errors
}

func (g *Gateway) backoff(attempt int) time.Duration {
	base := g.cfg.Retry.BackoffMS
	if base <= 0 {
		base = 200
	}
	d := time.Duration(base) * time.Millisecond
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

// dispatchUnary tries each target (primary then fallbacks), retrying retryable
// errors on each, and returns the first success along with the provider name
// that served it (for route-aware cost attribution) and whether that provider
// was served via the account's own key (BYOK — for the metering decision).
//
// BYOK is resolved PER TARGET: each provider needs the right key (an account's
// OpenAI key must not be sent to Anthropic), and on failover the metering
// decision follows the provider that actually served the request.
func (g *Gateway) dispatchUnary(ctx context.Context, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) (*openai.ChatCompletionResponse, string, bool, error) {
	var lastErr error
	for _, t := range res.All() {
		// Sovereignty gate: never open a connection to a non-local provider the
		// operator hasn't opted in. A blocked target is skipped so a local
		// fallback can still serve; if every target is blocked the 403 surfaces.
		if err := g.enforceSovereignty(t.Provider.Name()); err != nil {
			lastErr = err
			continue
		}
		callCtx, byok := g.resolveCredential(ctx, t.Provider.Name())
		for attempt := 0; attempt <= g.cfg.Retry.MaxRetries; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, "", false, ctx.Err()
				case <-time.After(g.backoff(attempt - 1)):
				}
			}
			resp, err := t.Provider.ChatCompletion(callCtx, req, t.Model, raw)
			if err == nil {
				return resp, t.Provider.Name(), byok, nil
			}
			lastErr = err
			if !shouldFailover(err) {
				return nil, "", false, err // client error: don't retry or fall over
			}
		}
		// Exhausted retries for this target; move to the next fallback.
	}
	return nil, "", false, lastErr
}
