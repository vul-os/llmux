package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"
)

// defaultAddr is the gateway base URL used by client subcommands.
const defaultAddr = "http://localhost:4000"

// httpGet performs a GET against addr+path with an optional bearer token and
// decodes the JSON body into v. It is the single network seam for the client
// subcommands so they stay testable against httptest servers.
func httpGet(addr, path, key string, v any) error {
	req, err := http.NewRequest(http.MethodGet, addr+path, nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", req.Method, path, resp.Status, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// modelsResponse mirrors GET /v1/models (openai.ModelList).
type modelsResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID            string  `json:"id"`
		InputPrice    float64 `json:"input_price_per_mtok"`
		OutputPrice   float64 `json:"output_price_per_mtok"`
		ContextWindow int     `json:"context_window"`
	} `json:"data"`
}

// fetchModels prints a readable table of models with pricing and context window.
func fetchModels(addr string, w io.Writer) error {
	var resp modelsResponse
	if err := httpGet(addr, "/v1/models", "", &resp); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tINPUT $/Mtok\tOUTPUT $/Mtok\tCONTEXT")
	for _, m := range resp.Data {
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%d\n", m.ID, m.InputPrice, m.OutputPrice, m.ContextWindow)
	}
	return tw.Flush()
}

// catalogResponse mirrors GET /v1/catalog.json.
type catalogResponse struct {
	Updated string `json:"updated"`
	Count   int    `json:"count"`
}

// fetchCatalog prints the catalog model count and the updated timestamp.
func fetchCatalog(addr string, w io.Writer) error {
	var resp catalogResponse
	if err := httpGet(addr, "/v1/catalog.json", "", &resp); err != nil {
		return err
	}
	fmt.Fprintf(w, "models:  %d\n", resp.Count)
	fmt.Fprintf(w, "updated: %s\n", resp.Updated)
	return nil
}

// keysResponse mirrors GET /admin/keys (server keyStatus list).
type keysResponse struct {
	Keys []struct {
		Name      string  `json:"name"`
		Key       string  `json:"key"`
		BudgetUSD float64 `json:"budget_usd"`
		SpendUSD  float64 `json:"spend_usd"`
		RPM       int     `json:"rpm"`
	} `json:"keys"`
}

// fetchKeys prints a table of virtual keys: name, masked key, budget, spend, rpm.
func fetchKeys(addr, key string, w io.Writer) error {
	var resp keysResponse
	if err := httpGet(addr, "/admin/keys", key, &resp); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKEY\tBUDGET $\tSPEND $\tRPM")
	for _, k := range resp.Keys {
		fmt.Fprintf(tw, "%s\t%s\t%.2f\t%.2f\t%d\n", k.Name, k.Key, k.BudgetUSD, k.SpendUSD, k.RPM)
	}
	return tw.Flush()
}
