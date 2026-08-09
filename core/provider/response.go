package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

// SetJSONFields rewrites top-level fields of a JSON object body in a SINGLE
// unmarshal/marshal pass, preserving every other field verbatim. Used on the
// request hot path to swap "model"/"stream" without re-parsing the whole body
// multiple times. Returns the original bytes unchanged if they aren't a JSON
// object. It drops nothing — callers that must honor the gateway's drop_params
// list use Options.SetJSONFields.
func SetJSONFields(raw []byte, fields map[string]any) []byte {
	return setJSONFields(raw, fields, nil)
}

// SetJSONFields rewrites top-level fields as SetJSONFields does, and also drops
// every key in this gateway's DropParams list.
func (o Options) SetJSONFields(raw []byte, fields map[string]any) []byte {
	return setJSONFields(raw, fields, o.DropParams)
}

func setJSONFields(raw []byte, fields map[string]any, drop []string) []byte {
	var m map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return raw
		}
	} else {
		m = map[string]json.RawMessage{}
	}
	for k, v := range fields {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		m[k] = b
	}
	for _, k := range drop {
		delete(m, k)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// NormalizeErrorType maps an HTTP status to OpenAI's canonical error `type`, so
// clients that branch on error.type behave consistently across providers.
func NormalizeErrorType(status int) string {
	switch {
	case status == 400 || status == 422:
		return "invalid_request_error"
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// IsContextLengthError reports whether an upstream error (given its HTTP status
// and message) looks like a context-length / token-window overflow, so adapters
// can set the OpenAI error code "context_length_exceeded". Heuristic: only on
// 400/413, where the lower-cased message mentions a context/token concept
// together with a length/window/maximum signal.
func IsContextLengthError(status int, message string) bool {
	if status != 400 && status != 413 {
		return false
	}
	m := strings.ToLower(message)
	hasSubject := strings.Contains(m, "context") || strings.Contains(m, "token")
	if !hasSubject {
		return false
	}
	return strings.Contains(m, "length") ||
		strings.Contains(m, "window") ||
		strings.Contains(m, "maximum") ||
		strings.Contains(m, "too long")
}

// ParseDataURI splits a data: URI into its media type and payload, reporting
// whether it is base64-encoded. ok is false for non-data URIs.
func ParseDataURI(uri string) (mediaType, data string, isBase64, ok bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false, false
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return "", "", false, false
	}
	meta := uri[5:comma]
	data = uri[comma+1:]
	mediaType = meta
	if semi := strings.IndexByte(meta, ';'); semi >= 0 {
		mediaType = meta[:semi]
		isBase64 = strings.Contains(meta[semi:], "base64")
	}
	return mediaType, data, isBase64, true
}

// Body returns the response body, size-limited when o.MaxResponseBytes > 0.
func (o Options) Body(resp *http.Response) io.Reader {
	if o.MaxResponseBytes > 0 {
		return io.LimitReader(resp.Body, o.MaxResponseBytes)
	}
	return resp.Body
}

// ---------------------------------------------------------------------------
// Header relay
// ---------------------------------------------------------------------------

// HeaderSink collects allow-listed upstream response headers so the gateway can
// relay them to the client (e.g. rate-limit headers), preserving OpenAI-contract
// fidelity. Providers call Capture; the handler copies Header() onto the client
// response. Concurrency-safe.
type HeaderSink struct {
	mu sync.Mutex
	h  http.Header
}

type sinkKeyT struct{}

var sinkKey sinkKeyT

// WithHeaderSink attaches a fresh sink to the context and returns both.
func WithHeaderSink(ctx context.Context) (context.Context, *HeaderSink) {
	s := &HeaderSink{h: http.Header{}}
	return context.WithValue(ctx, sinkKey, s), s
}

// SinkFrom returns the sink from ctx, or nil.
func SinkFrom(ctx context.Context) *HeaderSink {
	s, _ := ctx.Value(sinkKey).(*HeaderSink)
	return s
}

// relayPrefixes/relayExact are the headers safe and useful to pass through.
var relayPrefixes = []string{"x-ratelimit-"}
var relayExact = map[string]bool{"retry-after": true}

// Capture copies allow-listed headers from src into the sink.
func (s *HeaderSink) Capture(src http.Header) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, vs := range src {
		lk := strings.ToLower(k)
		keep := relayExact[lk]
		if !keep {
			for _, p := range relayPrefixes {
				if strings.HasPrefix(lk, p) {
					keep = true
					break
				}
			}
		}
		if keep {
			for _, v := range vs {
				s.h.Add(k, v)
			}
		}
	}
}

// Header returns a copy of the captured headers.
func (s *HeaderSink) Header() http.Header {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := http.Header{}
	for k, vs := range s.h {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
