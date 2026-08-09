package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/vul-os/llmux/core/gateway"
	"github.com/vul-os/llmux/core/openai"
	"github.com/vul-os/llmux/core/router"
)

// maxBodyBytes caps request bodies (generous for large multimodal/tool payloads).
const maxBodyBytes = 32 << 20 // 32 MiB

// handleChat is the HTTP adapter over Gateway.ChatRaw / Gateway.ChatStreamSink.
// It reads the body, decodes it, hands the ORIGINAL bytes to the gateway (so
// passthrough keeps unknown upstream fields), and maps the outcome onto HTTP.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("failed to read request body", "invalid_request_error", ""))
		return
	}

	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("invalid JSON: "+err.Error(), "invalid_request_error", ""))
		return
	}

	res, err := s.gw.Prepare(r.Context(), req.Model)
	if err != nil {
		writePrepareError(w, req.Model, err)
		return
	}

	if req.Stream {
		s.streamChat(w, r, &req, raw, res)
		return
	}

	out, err := s.gw.ChatRoute(r.Context(), &req, raw, res)
	if err != nil {
		if out != nil {
			relayHeaders(w, out.Headers) // relay rate-limit/retry-after headers even on error
		}
		s.writeProviderError(w, err)
		return
	}
	if out.CacheHit {
		w.Header().Set("X-LLMux-Cache", "hit")
	}
	relayHeaders(w, out.Headers)
	writeRawJSON(w, out.Body) // pre-serialized body; no re-marshal
}

// writePrepareError maps the pre-flight failures (missing model, key allow-list,
// unknown model, unmeterable-on-a-budgeted-key) onto their HTTP statuses.
func writePrepareError(w http.ResponseWriter, model string, err error) {
	var unmeterable *gateway.UnmeterableError
	switch {
	case errors.Is(err, gateway.ErrNoModel):
		writeError(w, http.StatusBadRequest,
			openai.NewError("you must provide a model parameter", "invalid_request_error", "missing_model"))
	case errors.Is(err, gateway.ErrModelNotAllowed):
		writeError(w, http.StatusForbidden,
			openai.NewError("model "+model+" not allowed for this key", "invalid_request_error", "model_not_allowed"))
	case errors.As(err, &unmeterable):
		// Fail closed: never serve a metered request on a budgeted key for a model
		// we cannot price — the spend would go uncounted and the budget never trip.
		writeUnmeterable(w, model)
	default:
		writeError(w, http.StatusNotFound,
			openai.NewError(err.Error(), "invalid_request_error", "model_not_found"))
	}
}

// writeUnmeterable refuses a request the metering guard flagged, with a stable
// error code an operator can act on (price the model, or scope the key).
func writeUnmeterable(w http.ResponseWriter, model string) {
	writeError(w, http.StatusForbidden, openai.NewError(
		"model "+model+" has no configured price; refusing to serve an unmeterable request against a budgeted key",
		"invalid_request_error", "model_not_priced"))
}

// relayHeaders copies captured upstream headers (rate-limit, retry-after) onto
// the client response.
func relayHeaders(w http.ResponseWriter, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// streamChat is the SSE adapter over Gateway.ChatStreamSink. The gateway owns
// failover, metering and the usage-chunk fallback; the sink owns the wire
// format. Headers are written only when the first chunk actually arrives, so a
// failure before the stream starts is still an ordinary JSON error response.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req *openai.ChatCompletionRequest, raw []byte, res router.Resolution) {
	sink := &sseSink{w: w}
	err := s.gw.ChatStreamSink(r.Context(), req, raw, res, sink)
	if err != nil && !sink.started() {
		s.writeProviderError(w, err)
	}
	// A failure AFTER the stream started was already relayed as an SSE error
	// event and metered by the gateway; nothing more can be written here.
}

// sseSink adapts the SSE writer to gateway.StreamSink.
type sseSink struct {
	w   http.ResponseWriter
	sse *sseWriter
}

// Open writes the streaming headers. It fails when the ResponseWriter cannot
// flush (no streaming possible), which aborts before anything is served.
func (s *sseSink) Open() error {
	sse, ok := newSSEWriter(s.w)
	if !ok {
		return io.ErrClosedPipe
	}
	s.sse = sse
	return nil
}

// Chunk writes one chat completion chunk as an SSE data event.
func (s *sseSink) Chunk(c *openai.ChatCompletionChunk) error { return s.sse.chunk(c) }

// Failed writes an OpenAI-shaped error as an SSE event (the stream had already
// started, so the status line is long gone).
func (s *sseSink) Failed(e *openai.ErrorResponse) {
	if s.sse != nil {
		s.sse.errorEvent(e)
	}
}

// Done writes the terminal [DONE] sentinel, opening the stream first if no
// chunk ever arrived so the client still sees a valid terminal stream.
func (s *sseSink) Done() {
	if s.sse == nil {
		if err := s.Open(); err != nil {
			return
		}
	}
	s.sse.done()
}

// started reports whether the response was committed (headers written).
func (s *sseSink) started() bool { return s.sse != nil }
