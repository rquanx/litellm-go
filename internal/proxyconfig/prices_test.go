package proxyconfig

import (
	"math"
	"strings"
	"testing"
)

func TestConfiguredCostBasedRoutingPrices(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: example
    litellm_params: { model: openai/gpt-4o }
  - model_name: example
    litellm_params: { model: openai/gpt-4o-mini, input_cost_per_token: 0 }
  - model_name: example
    litellm_params: { model: openai/unknown }
router_settings:
  routing_strategy: cost-based-routing
`))
	if err != nil {
		t.Fatal(err)
	}
	prices := []float64{1.25e-5, 6e-7, 10}
	for index, candidate := range config.Routes[0].Candidates {
		if math.Abs(candidate.TokenPrice-prices[index]) > 1e-12 {
			t.Errorf("%s: configured token price %g, want %g", candidate.ModelID, candidate.TokenPrice, prices[index])
		}
	}
}
