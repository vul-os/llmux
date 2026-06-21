package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// ParseOpenRouter parses the OpenRouter /models response into prices. Its
// pricing fields are per-token USD strings; we convert to per-MTok.
func ParseOpenRouter(data []byte) (map[string]Price, error) {
	var doc struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			TopProvider struct {
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			Architecture struct {
				Modality string `json:"modality"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("openrouter parse: %w", err)
	}
	out := make(map[string]Price, len(doc.Data))
	for _, m := range doc.Data {
		provider := ""
		if i := strings.IndexByte(m.ID, '/'); i != -1 {
			provider = m.ID[:i]
		}
		var caps []string
		for _, p := range m.SupportedParameters {
			if p == "tools" {
				caps = append(caps, "tools")
			}
		}
		if strings.Contains(m.Architecture.Modality, "image") {
			caps = append(caps, "vision")
		}
		out[m.ID] = Price{
			Model: m.ID, Provider: provider,
			InputPerMTok:  perTokenToMTok(m.Pricing.Prompt),
			OutputPerMTok: perTokenToMTok(m.Pricing.Completion),
			ContextWindow: m.ContextLength,
			MaxOutput:     m.TopProvider.MaxCompletionTokens,
			Capabilities:  caps,
		}
	}
	return out, nil
}

// ParseLiteLLM parses LiteLLM's model_prices_and_context_window.json. Its costs
// are per-token USD floats; we convert to per-MTok.
func ParseLiteLLM(data []byte) (map[string]Price, error) {
	var doc map[string]struct {
		MaxInputTokens          int     `json:"max_input_tokens"`
		MaxOutputTokens         int     `json:"max_output_tokens"`
		InputCostPerToken       float64 `json:"input_cost_per_token"`
		OutputCostPerToken      float64 `json:"output_cost_per_token"`
		CacheReadInputTokenCost float64 `json:"cache_read_input_token_cost"`
		LiteLLMProvider         string  `json:"litellm_provider"`
		Mode                    string  `json:"mode"`
		SupportsFunctionCalling bool    `json:"supports_function_calling"`
		SupportsVision          bool    `json:"supports_vision"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("litellm parse: %w", err)
	}
	out := make(map[string]Price, len(doc))
	for id, m := range doc {
		if id == "sample_spec" || m.Mode != "" && m.Mode != "chat" && m.Mode != "completion" {
			continue
		}
		var caps []string
		if m.SupportsFunctionCalling {
			caps = append(caps, "tools")
		}
		if m.SupportsVision {
			caps = append(caps, "vision")
		}
		out[id] = Price{
			Model: id, Provider: m.LiteLLMProvider,
			InputPerMTok:     m.InputCostPerToken * 1e6,
			OutputPerMTok:    m.OutputCostPerToken * 1e6,
			CacheReadPerMTok: m.CacheReadInputTokenCost * 1e6,
			ContextWindow:    m.MaxInputTokens,
			MaxOutput:        m.MaxOutputTokens,
			Capabilities:     caps,
		}
	}
	return out, nil
}

// ParseAzure parses the Azure Retail Prices API response for Azure OpenAI.
//
// Best-effort: Azure exposes prices as per-meter line items (e.g. "gpt-4o Input
// Global Tokens" priced per 1K tokens), not a clean model catalog. We derive the
// model name and input/output side from the meter name and normalize to per-MTok.
// This mapping is heuristic and should be verified against the live API before
// relying on it; pagination (NextPageLink) is not followed here.
func ParseAzure(data []byte) (map[string]Price, error) {
	var doc struct {
		Items []struct {
			UnitPrice     float64 `json:"unitPrice"`
			RetailPrice   float64 `json:"retailPrice"`
			ProductName   string  `json:"productName"`
			MeterName     string  `json:"meterName"`
			SkuName       string  `json:"skuName"`
			UnitOfMeasure string  `json:"unitOfMeasure"`
		} `json:"Items"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("azure parse: %w", err)
	}
	out := map[string]Price{}
	for _, it := range doc.Items {
		meter := strings.ToLower(it.MeterName)
		isOutput := strings.Contains(meter, "output")
		isInput := strings.Contains(meter, "input")
		if !isInput && !isOutput {
			continue // skip non-token meters (images, fine-tuning, etc.)
		}
		model := azureModelFromMeter(it.MeterName)
		if model == "" {
			continue
		}
		perMTok := it.UnitPrice * unitMultiplier(it.UnitOfMeasure)
		id := "azure/" + model
		p := out[id]
		p.Model, p.Provider = id, "azure"
		if isOutput {
			p.OutputPerMTok = perMTok
		} else {
			p.InputPerMTok = perMTok
		}
		out[id] = p
	}
	return out, nil
}

// azureModelFromMeter extracts a model slug from an Azure meter name like
// "gpt-4o Input Global Tokens" -> "gpt-4o".
func azureModelFromMeter(meter string) string {
	m := strings.ToLower(meter)
	for _, w := range []string{"input", "output", "global", "regional", "data zone", "tokens", "cached", "tokn"} {
		m = strings.ReplaceAll(m, w, " ")
	}
	return strings.Join(strings.Fields(m), "-")
}

// unitMultiplier converts an Azure unitOfMeasure to a per-MTok multiplier.
func unitMultiplier(unit string) float64 {
	u := strings.ToLower(strings.TrimSpace(unit))
	switch {
	case strings.HasPrefix(u, "1m"), strings.Contains(u, "1000000"):
		return 1
	case strings.HasPrefix(u, "1k"), strings.Contains(u, "1000"):
		return 1000
	case u == "1", u == "1 unit":
		return 1e6
	default:
		return 1000 // Azure token meters are most commonly per-1K
	}
}

func perTokenToMTok(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f * 1e6
}

// Syncer periodically refreshes a Catalog from a set of pricing sources.
type Syncer struct {
	catalog   *Catalog
	sources   []Source
	interval  time.Duration
	cachePath string
}

// NewSyncer builds a Syncer. cachePath (optional) persists the merged catalog
// for warm starts.
func NewSyncer(cat *Catalog, sources []Source, interval time.Duration, cachePath string) *Syncer {
	return &Syncer{catalog: cat, sources: sources, interval: interval, cachePath: cachePath}
}

// SyncOnce fetches every source and updates the catalog per-source.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	var firstErr error
	for _, src := range s.sources {
		prices, err := src.Fetch(ctx)
		if err != nil {
			log.Printf("llmux: pricing source %s: %v", src.Name(), err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.catalog.SetSource(src.Name(), src.Priority(), prices)
		log.Printf("llmux: pricing source %s: %d models", src.Name(), len(prices))
	}
	if s.cachePath != "" {
		if err := s.catalog.Save(s.cachePath); err != nil {
			log.Printf("llmux: pricing cache save: %v", err)
		}
	}
	return firstErr
}

// Run does an initial sync then refreshes on the interval until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	if err := s.SyncOnce(ctx); err != nil {
		log.Printf("llmux: initial pricing sync had errors (using built-in/cached catalog): %v", err)
	}
	if s.interval <= 0 {
		return
	}
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.SyncOnce(ctx)
		}
	}
}
