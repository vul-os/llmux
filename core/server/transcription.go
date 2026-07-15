package server

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/llmux/llmux/core/openai"
	"github.com/llmux/llmux/core/provider"
)

// transcriptionRoutes maps OpenAI audio-INPUT routes (speech-to-text) to the
// upstream path suffix. Unlike modalityRoutes (forward.go), these carry a
// MULTIPART form body — an uploaded audio file plus a "model" form field — not a
// JSON body with a "model" key. They therefore need a multipart-aware handler
// that reads and rewrites the model FORM field and re-encodes the upload before
// forwarding. Server-side voice transcription (e.g. live captions) routes here.
var transcriptionRoutes = map[string]string{
	"POST /v1/audio/transcriptions": "/audio/transcriptions",
	"POST /v1/audio/translations":   "/audio/translations",
}

// registerTranscriptionRoutes wires the multipart audio-input forwarders.
func (s *Server) registerTranscriptionRoutes() {
	for pattern, suffix := range transcriptionRoutes {
		suffix := suffix
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.handleTranscription(w, r, suffix)
		})
	}
}

// handleTranscription proxies an OpenAI audio-input request (transcriptions /
// translations) to the routed provider. The body is multipart/form-data, so the
// model is read from — and rewritten in — a form field, and the multipart body
// is re-encoded before forwarding. All the usual gates apply exactly as for the
// JSON modality routes: per-key model allow-list, routing, the fail-closed
// meterability gate, sovereignty egress, and BYOK-vs-central credential choice.
//
// METERING NOTE — per-audio-minute is a KNOWN GAP, not a silent free path:
// Whisper-class transcription responses carry no token-usage object, and llmux
// has no per-audio-minute price in the catalog, so a SERVED transcription is
// recorded as a $0 AUDITABLE usage line (model + account present in the log),
// never dropped. A BUDGETED key is still fully protected: with no catalog price,
// unmeterableBudgeted refuses the request PRE-FLIGHT — exactly like any other
// unpriceable model — so a budget can never be evaded via audio. Wiring true
// per-audio-minute pricing (so unbudgeted/central audio is billed by duration)
// is a pending pricing decision; until then audio meters $0 for unbudgeted/BYOK
// keys and is hard-refused for budgeted keys.
func (s *Server) handleTranscription(w http.ResponseWriter, r *http.Request, suffix string) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") || boundary == "" {
		writeError(w, http.StatusBadRequest, openai.NewError(
			"audio endpoints require a multipart/form-data body with a boundary",
			"invalid_request_error", "invalid_content_type"))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("failed to read request body", "invalid_request_error", ""))
		return
	}

	// Read the model form field (upstream target not known until the route resolves).
	model := scanMultipartModel(raw, boundary)
	if model == "" {
		writeError(w, http.StatusBadRequest, openai.NewError("you must provide a model parameter", "invalid_request_error", "missing_model"))
		return
	}
	if k := keyFrom(r.Context()); k != nil && !k.AllowsModel(model) {
		writeError(w, http.StatusForbidden, openai.NewError("model "+model+" not allowed for this key", "invalid_request_error", "model_not_allowed"))
		return
	}
	res, err := s.router.Resolve(model)
	if err != nil {
		writeError(w, http.StatusNotFound, openai.NewError(err.Error(), "invalid_request_error", "model_not_found"))
		return
	}
	// Fail closed: never serve a metered request on a budgeted key for a model we
	// cannot price (see unmeterableBudgeted) — uncounted audio spend would evade
	// the budget. Transcription models are unpriced in the catalog today, so a
	// budgeted key is refused here rather than burning unbounded upstream spend.
	if s.unmeterableBudgeted(r.Context(), model, res.Primary.Provider.Name()) {
		writeUnmeterable(w, model)
		return
	}
	t := res.Primary
	fwd, ok := t.Provider.(provider.Forwarder)
	if !ok {
		writeError(w, http.StatusNotImplemented, openai.NewError(
			"provider "+t.Provider.Name()+" does not support "+suffix, "invalid_request_error", "unsupported_endpoint"))
		return
	}
	// Sovereignty gate: audio egress obeys the same default-deny as chat — never
	// open a socket to a non-local provider the operator hasn't opted in.
	if err := s.enforceSovereignty(t.Provider.Name()); err != nil {
		if pe, ok := err.(*provider.Error); ok {
			writeError(w, pe.StatusCode, pe.Body)
		} else {
			writeError(w, http.StatusForbidden, openai.NewError(err.Error(), "sovereignty_error", "egress_not_allowed"))
		}
		return
	}

	// Re-encode the multipart body with the model field rewritten to the upstream
	// target name, preserving the uploaded file and every other field verbatim.
	body, contentType, err := rewriteMultipartModel(raw, boundary, t.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, openai.NewError("malformed multipart body", "invalid_request_error", "invalid_multipart"))
		return
	}

	// BYOK vs central for the routed provider: inject the account's own key when
	// set, and mark the forward unmetered.
	callCtx, byok := s.resolveCredential(r.Context(), t.Provider.Name())
	fr, err := fwd.Forward(callCtx, provider.ForwardRequest{
		Method: http.MethodPost, Suffix: suffix, Body: body, ContentType: contentType,
	})
	if err != nil {
		s.metrics.incUpstreamErr()
		writeProviderError(w, err)
		return
	}
	defer fr.Body.Close()
	status := fr.Status
	// Relay the response while tapping any usage the upstream reported. Errors
	// carry no spend, so they are relayed but not metered.
	usage, _ := copyForwardMetered(w, fr)
	if status < 200 || status >= 300 {
		return
	}
	// Transcription responses usually carry no usage object; record a $0 auditable
	// line (model present) so the request is never a silent free path. When a
	// provider DOES report usage (e.g. gpt-4o-transcribe), it is metered normally.
	if usage == nil {
		usage = &openai.Usage{}
	}
	s.attachCost(model, t.Provider.Name(), usage)
	meterCtx := withBYOK(r.Context(), byok)
	s.recordSpend(meterCtx, usage)
	s.logUsage(meterCtx, model, false, false, usage)
}

// scanMultipartModel reads ONLY the "model" form field from a multipart body,
// discarding file bytes. Best-effort: returns "" if the field is absent or the
// body is malformed. Used to resolve routing before the full re-encode.
func scanMultipartModel(raw []byte, boundary string) string {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if part.FormName() == "model" && part.FileName() == "" {
			data, _ := io.ReadAll(io.LimitReader(part, 1<<12))
			part.Close()
			return strings.TrimSpace(string(data))
		}
		part.Close()
	}
}

// rewriteMultipartModel re-encodes a multipart/form-data body with the "model"
// field replaced by target (when target != ""), preserving the uploaded file
// (its name, filename, and content-type) and every other field verbatim. It
// returns the new body and its multipart Content-Type (a fresh boundary is
// generated, so callers must use the returned content type, not the original).
func rewriteMultipartModel(raw []byte, boundary, target string) ([]byte, string, error) {
	mr := multipart.NewReader(bytes.NewReader(raw), boundary)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "model" && part.FileName() == "" {
			val := target
			if val == "" {
				data, _ := io.ReadAll(io.LimitReader(part, 1<<12))
				val = strings.TrimSpace(string(data))
			}
			part.Close()
			if err := mw.WriteField("model", val); err != nil {
				return nil, "", err
			}
			continue
		}
		// Copy the part verbatim, preserving its MIME header (Content-Disposition
		// with name/filename, and any Content-Type on a file part).
		hdr := make(textproto.MIMEHeader, len(part.Header))
		for k, vs := range part.Header {
			for _, v := range vs {
				hdr.Add(k, v)
			}
		}
		pw, err := mw.CreatePart(hdr)
		if err != nil {
			part.Close()
			return nil, "", err
		}
		if _, err := io.Copy(pw, part); err != nil {
			part.Close()
			return nil, "", err
		}
		part.Close()
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
