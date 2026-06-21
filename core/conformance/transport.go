// Package conformance is the provider verification harness. It records real
// provider HTTP responses to fixtures, then replays them in CI so adapter
// translation is checked against REAL responses — not against our own mocks of
// what we think the API does. This is what lets a provider be promoted to
// "stable" (see SUPPORT.md / HARDENING.md).
//
// Modes:
//   - Record: forward to the real API, tee the response into a fixture file.
//   - Replay: serve the recorded fixture; no network. Missing fixture -> ErrNoFixture.
//   - Live:   forward to the real API, record nothing.
//
// A Transport is installed as the RoundTripper on the provider's HTTP clients.
package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Mode selects record/replay/live behavior.
type Mode string

const (
	Replay Mode = "replay"
	Record Mode = "record"
	Live   Mode = "live"
)

// ErrNoFixture is returned in Replay mode when a case has no recorded fixture.
// Runners should treat it as a skip ("record fixtures with real keys first").
var ErrNoFixture = errors.New("conformance: no fixture recorded")

// fixture is the serialized form of a captured HTTP response.
type fixture struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	BodyB64     string `json:"body_b64"`
}

// Transport is an http.RoundTripper that records or replays fixtures. Set Case
// before each request so the fixture is keyed deterministically by case name.
type Transport struct {
	Mode Mode
	Dir  string            // fixtures root (e.g. testdata/fixtures)
	Real http.RoundTripper // used in Record/Live (defaults to http.DefaultTransport)

	mu  sync.Mutex
	cse string // current case key
}

// SetCase sets the fixture key for subsequent requests.
func (t *Transport) SetCase(name string) {
	t.mu.Lock()
	t.cse = name
	t.mu.Unlock()
}

func (t *Transport) caseName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cse
}

func (t *Transport) real() http.RoundTripper {
	if t.Real != nil {
		return t.Real
	}
	return http.DefaultTransport
}

func (t *Transport) path() string {
	return filepath.Join(t.Dir, t.caseName()+".json")
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.Mode {
	case Live:
		return t.real().RoundTrip(req)
	case Record:
		return t.record(req)
	default:
		return t.replay(req)
	}
}

func (t *Transport) record(req *http.Request) (*http.Response, error) {
	resp, err := t.real().RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	fx := fixture{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		BodyB64:     base64.StdEncoding.EncodeToString(body),
	}
	data, _ := json.MarshalIndent(fx, "", "  ")
	if err := os.MkdirAll(filepath.Dir(t.path()), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(t.path(), data, 0o644); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (t *Transport) replay(req *http.Request) (*http.Response, error) {
	data, err := os.ReadFile(t.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoFixture
		}
		return nil, err
	}
	var fx fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		return nil, err
	}
	body, err := base64.StdEncoding.DecodeString(fx.BodyB64)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	if fx.ContentType != "" {
		h.Set("Content-Type", fx.ContentType)
	}
	return &http.Response{
		StatusCode: fx.Status,
		Status:     http.StatusText(fx.Status),
		Header:     h,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// HasFixture reports whether a fixture exists for the given case.
func (t *Transport) HasFixture(name string) bool {
	_, err := os.Stat(filepath.Join(t.Dir, name+".json"))
	return err == nil
}
