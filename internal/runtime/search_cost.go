package runtime

import "encoding/json"

// Prices are from model_prices_and_context_window.json at LiteLLM v1.85.0.
func searchCost(provider string, input map[string]json.RawMessage, queryCount int) *float64 {
	limit := 10
	if raw := input["max_results"]; len(raw) != 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &limit); err != nil {
			return nil
		}
	}
	var rate float64
	switch provider {
	case "perplexity", "google_pse":
		rate = 0.005
	case "dataforseo":
		rate = 0.003
	case "tavily":
		rate = 0.008
	case "linkup":
		rate = 0.00587
	case "serper":
		rate = 0.001
	case "searxng", "duckduckgo":
		rate = 0
	case "parallel_ai":
		rate = 0.004
	case "exa_ai":
		rate = 0.025
		if limit >= 0 && limit <= 25 {
			rate = 0.005
		}
	case "firecrawl":
		rate = 0.0166
		if limit >= 1 && limit <= 100 {
			rate = 0.00166 * float64((limit+9)/10)
		}
	default:
		return nil
	}
	cost := rate * float64(queryCount)
	return &cost
}
