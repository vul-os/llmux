package server

import (
	"context"
	"net/http"
	"time"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/router"
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
	if pe := asProviderError(err); pe != nil {
		return retryableStatus(pe.Status())
	}
	return true // transport-level errors
}

func (s *Server) backoff(attempt int) time.Duration {
	base := s.cfg.Retry.BackoffMS
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
// that served it (for route-aware cost attribution).
func (s *Server) dispatchUnary(ctx context.Context, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) (*openai.ChatCompletionResponse, string, error) {
	var lastErr error
	for _, t := range res.All() {
		for attempt := 0; attempt <= s.cfg.Retry.MaxRetries; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, "", ctx.Err()
				case <-time.After(s.backoff(attempt - 1)):
				}
			}
			resp, err := t.Provider.ChatCompletion(ctx, req, t.Model, raw)
			if err == nil {
				return resp, t.Provider.Name(), nil
			}
			lastErr = err
			if !shouldFailover(err) {
				return nil, "", err // client error: don't retry or fall over
			}
		}
		// Exhausted retries for this target; move to the next fallback.
	}
	return nil, "", lastErr
}
