// Package router resolves a client-facing model name to a concrete provider and
// upstream model. Resolution order: explicit config routes (exact before "*"),
// then "provider/model" prefix syntax. Fallback chains are attached for Wave 3.
package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/llmux/llmux/core/config"
	"github.com/llmux/llmux/core/provider"
)

// Pricer supplies per-MTok rates for least-cost routing.
type Pricer interface {
	Rate(model string) (in, out float64, ok bool)
}

// Target is one routable destination.
type Target struct {
	Provider provider.Provider
	// Model is the upstream model name to send.
	Model string
}

// Resolution is the primary target plus ordered fallbacks.
type Resolution struct {
	Primary   Target
	Fallbacks []Target
}

// All returns primary followed by fallbacks, in attempt order.
func (r Resolution) All() []Target {
	return append([]Target{r.Primary}, r.Fallbacks...)
}

// Router resolves model names using the configured routes and registry.
type Router struct {
	routes   []config.RouteConfig
	registry *provider.Registry
	pricer   Pricer
}

// New builds a Router. pricer may be nil (disables least-cost routing).
func New(routes []config.RouteConfig, reg *provider.Registry, pricer Pricer) *Router {
	return &Router{routes: routes, registry: reg, pricer: pricer}
}

// Resolve maps a client model name to a Resolution, or returns an error if the
// model cannot be routed to any registered provider.
func (r *Router) Resolve(model string) (Resolution, error) {
	if rc, remainder, ok := r.matchRoute(model); ok {
		return r.build(rc, model, remainder)
	}
	// "provider/model" prefix syntax, e.g. "openai/gpt-4o".
	if name, rest, found := strings.Cut(model, "/"); found {
		if p, ok := r.registry.Get(name); ok {
			return Resolution{Primary: Target{Provider: p, Model: rest}}, nil
		}
	}
	return Resolution{}, fmt.Errorf("no route for model %q (providers: %s)",
		model, strings.Join(r.registry.Names(), ", "))
}

// matchRoute returns the best matching route. Precedence: an exact model match
// wins over the longest matching trailing-"*" prefix pattern (e.g. "claude-*"),
// which in turn wins over the "*" catch-all. For a prefix match, remainder is
// the part of the requested model that followed the matched prefix (used for
// LiteLLM-style target substitution); it is "" for exact and catch-all matches.
func (r *Router) matchRoute(model string) (rc config.RouteConfig, remainder string, ok bool) {
	var catchAll *config.RouteConfig
	var bestPrefix *config.RouteConfig
	var bestPrefixLen = -1
	var bestRemainder string
	for i := range r.routes {
		m := r.routes[i].Model
		switch {
		case m == model:
			return r.routes[i], "", true
		case m == "*":
			if catchAll == nil {
				catchAll = &r.routes[i]
			}
		case strings.HasSuffix(m, "*") && !strings.HasPrefix(m, "*"):
			prefix := strings.TrimSuffix(m, "*")
			if strings.HasPrefix(model, prefix) && len(prefix) > bestPrefixLen {
				bestPrefix = &r.routes[i]
				bestPrefixLen = len(prefix)
				bestRemainder = strings.TrimPrefix(model, prefix)
			}
		}
	}
	if bestPrefix != nil {
		return *bestPrefix, bestRemainder, true
	}
	if catchAll != nil {
		return *catchAll, "", true
	}
	return config.RouteConfig{}, "", false
}

func (r *Router) build(rc config.RouteConfig, requested, remainder string) (Resolution, error) {
	if rc.Strategy == "least-cost" && len(rc.Candidates) > 0 {
		return r.leastCost(rc)
	}

	p, ok := r.registry.Get(rc.Provider)
	if !ok {
		return Resolution{}, fmt.Errorf("route %q references unknown provider %q", rc.Model, rc.Provider)
	}
	isPrefix := strings.HasSuffix(rc.Model, "*") && rc.Model != "*"
	target := rc.TargetModel
	switch {
	case isPrefix:
		switch {
		case target == "":
			// No target: forward the full requested model unchanged.
			target = requested
		case strings.Contains(target, "*"):
			// Substitute the matched remainder into the target's "*"
			// (LiteLLM-style), e.g. "gpt-4*" -> "azure/gpt-4*", request
			// "gpt-4o" -> "azure/gpt-4o".
			target = strings.Replace(target, "*", remainder, 1)
		}
		// Otherwise use the literal TargetModel as-is.
	case target == "":
		// For a catch-all, forward the requested model unchanged.
		if rc.Model == "*" {
			target = requested
		} else {
			target = rc.Model
		}
	}
	res := Resolution{Primary: Target{Provider: p, Model: target}}
	for _, fb := range rc.Fallbacks {
		if fp, ok := r.registry.Get(fb); ok {
			res.Fallbacks = append(res.Fallbacks, Target{Provider: fp, Model: target})
		}
	}
	return res, nil
}

// leastCost orders candidates by total per-MTok rate (cheapest first) and uses
// the rest as fallbacks. Candidates with unknown pricing sort last.
func (r *Router) leastCost(rc config.RouteConfig) (Resolution, error) {
	type scored struct {
		target Target
		cost   float64
	}
	var list []scored
	for _, c := range rc.Candidates {
		p, ok := r.registry.Get(c.Provider)
		if !ok {
			continue
		}
		cost := 1e18 // unknown pricing sorts last
		if r.pricer != nil {
			if in, out, ok := r.pricer.Rate(c.Model); ok {
				cost = in + out
			}
		}
		list = append(list, scored{Target{Provider: p, Model: c.Model}, cost})
	}
	if len(list) == 0 {
		return Resolution{}, fmt.Errorf("least-cost route %q: no candidates resolve to a registered provider", rc.Model)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].cost < list[j].cost })
	res := Resolution{Primary: list[0].target}
	for _, s := range list[1:] {
		res.Fallbacks = append(res.Fallbacks, s.target)
	}
	return res, nil
}
