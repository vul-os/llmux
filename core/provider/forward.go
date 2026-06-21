package provider

import (
	"context"
	"io"
	"net/http"
)

// Forwarder is an optional capability: providers that can transparently proxy an
// arbitrary OpenAI-shaped resource path (images, audio, moderations, rerank,
// batches, files, ...) to their upstream. Passthrough providers implement this;
// translating adapters do not (those endpoints return 501 for them). This is how
// llmux covers the long tail of OpenAI endpoints cheaply.
type Forwarder interface {
	// Forward proxies a request to {baseURL}{suffix} with provider auth swapped
	// in, returning the upstream status, headers, and streamable body. The
	// caller owns closing Body.
	Forward(ctx context.Context, fr ForwardRequest) (*ForwardResponse, error)
}

// ForwardRequest carries everything needed to proxy a resource request.
type ForwardRequest struct {
	Method      string
	Suffix      string // path appended to baseURL, e.g. "/images/generations"
	Body        []byte
	ContentType string
}

// ForwardResponse is the upstream response to relay.
type ForwardResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}
