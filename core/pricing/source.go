package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Source priorities. Lower wins in the precedence-merged "best" view, so a
// manual override beats a provider pricing API, which beats the community
// LiteLLM file, which beats OpenRouter (margin-inclusive), which beats the
// built-in offline seed.
const (
	PriorityOverride    = 0
	PriorityProviderAPI = 10 // azure / bedrock / gcp
	PriorityLiteLLM     = 20
	PriorityOpenRouter  = 30
	PriorityBuiltin     = 100
)

// SourceNameOverride is the reserved source name for manual overrides, which
// the catalog treats as always-authoritative regardless of route.
const SourceNameOverride = "override"

// Source fetches a set of model prices. Its Name should match the provider name
// used for routing when the prices are route-specific (e.g. "openrouter",
// "azure"), so the catalog can apply them for calls routed through that
// provider. Priority controls precedence in the merged catalog.
type Source interface {
	Name() string
	Priority() int
	Fetch(ctx context.Context) (map[string]Price, error)
}

// httpSource fetches a URL and parses it with a format-specific parser.
type httpSource struct {
	name     string
	priority int
	url      string
	parse    func([]byte) (map[string]Price, error)
	client   *http.Client
}

func (s *httpSource) Name() string  { return s.name }
func (s *httpSource) Priority() int { return s.priority }

func (s *httpSource) Fetch(ctx context.Context) (map[string]Price, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return s.parse(data)
}

func defaultClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// NewOpenRouterSource builds the OpenRouter /models source.
func NewOpenRouterSource(url string) Source {
	if url == "" {
		url = "https://openrouter.ai/api/v1/models"
	}
	return &httpSource{name: "openrouter", priority: PriorityOpenRouter, url: url, parse: ParseOpenRouter, client: defaultClient()}
}

// NewLiteLLMSource builds the LiteLLM open-JSON source.
func NewLiteLLMSource(url string) Source {
	if url == "" {
		url = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	}
	return &httpSource{name: "litellm", priority: PriorityLiteLLM, url: url, parse: ParseLiteLLM, client: defaultClient()}
}

// NewAzureSource builds the Azure Retail Prices source for Azure OpenAI models.
func NewAzureSource() Source {
	// Public, unauthenticated retail price API filtered to Azure OpenAI.
	url := "https://prices.azure.com/api/retail/prices?$filter=" +
		"serviceName eq 'Azure OpenAI' and priceType eq 'Consumption'"
	return &httpSource{name: "azure", priority: PriorityProviderAPI, url: url, parse: ParseAzure, client: defaultClient()}
}

// SourceFromURL picks a source implementation by URL hint.
func SourceFromURL(url string) Source {
	switch {
	case strings.Contains(url, "openrouter"):
		return NewOpenRouterSource(url)
	case strings.Contains(url, "litellm"), strings.Contains(url, "model_prices"):
		return NewLiteLLMSource(url)
	default:
		// Unknown: try OpenRouter shape first, then LiteLLM.
		return &httpSource{name: hostName(url), priority: PriorityLiteLLM, url: url, client: defaultClient(),
			parse: func(b []byte) (map[string]Price, error) {
				if p, err := ParseOpenRouter(b); err == nil && len(p) > 0 {
					return p, nil
				}
				return ParseLiteLLM(b)
			}}
	}
}

func hostName(url string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i != -1 {
		u = u[:i]
	}
	return u
}

// staticSource serves a fixed price map (used for inline overrides and builtin).
type staticSource struct {
	name     string
	priority int
	prices   map[string]Price
}

func (s *staticSource) Name() string                                    { return s.name }
func (s *staticSource) Priority() int                                   { return s.priority }
func (s *staticSource) Fetch(context.Context) (map[string]Price, error) { return s.prices, nil }

// NewOverrideSource builds an always-authoritative inline override source.
func NewOverrideSource(prices map[string]Price) Source {
	return &staticSource{name: SourceNameOverride, priority: PriorityOverride, prices: prices}
}

// fileOverrideSource reads override prices from a JSON file at fetch time, so
// edits take effect on the next sync without a restart.
type fileOverrideSource struct{ path string }

func (s *fileOverrideSource) Name() string  { return SourceNameOverride }
func (s *fileOverrideSource) Priority() int { return PriorityOverride }

func (s *fileOverrideSource) Fetch(context.Context) (map[string]Price, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var prices map[string]Price
	if err := json.Unmarshal(data, &prices); err != nil {
		return nil, fmt.Errorf("override file %s: %w", s.path, err)
	}
	for id, p := range prices {
		if p.Model == "" {
			p.Model = id
			prices[id] = p
		}
	}
	return prices, nil
}

// NewFileOverrideSource builds an override source backed by a JSON file.
func NewFileOverrideSource(path string) Source { return &fileOverrideSource{path: path} }
