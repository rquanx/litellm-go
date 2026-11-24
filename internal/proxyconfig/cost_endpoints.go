package proxyconfig

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (registry *modelRegistry) costMapSource(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"source": "local", "url": nil, "is_env_forced": false,
		"fallback_reason": nil, "model_count": len(modelTokenPrices),
	})
}

type costInput struct {
	Model            string `json:"model"`
	ModelName        string `json:"model_name"`
	InputTokens      *int   `json:"input_tokens"`
	OutputTokens     *int   `json:"output_tokens"`
	PromptTokens     *int   `json:"prompt_tokens"`
	CompletionTokens *int   `json:"completion_tokens"`
	RequestsPerDay   *int   `json:"num_requests_per_day"`
	RequestsPerMonth *int   `json:"num_requests_per_month"`
}

func (input costInput) counts() (int, int) {
	var in, out int
	if input.InputTokens != nil {
		in = *input.InputTokens
	} else if input.PromptTokens != nil {
		in = *input.PromptTokens
	}
	if input.OutputTokens != nil {
		out = *input.OutputTokens
	} else if input.CompletionTokens != nil {
		out = *input.CompletionTokens
	}
	return in, out
}

func (registry *modelRegistry) costPrices(model string) (string, [2]*float64) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	group := model
	if alias, exists := registry.base.modelGroupAliases[model]; exists {
		group = alias.Model
	}
	for _, models := range [][]deployment{registry.base.models, registry.models} {
		for _, entry := range models {
			if entry.Alias != group && entry.Params.Model != model {
				continue
			}
			_, rates, err := tokenPrice(entry.Params.Model, entry.Params.InputPrice, entry.Params.OutputPrice)
			if err == nil {
				return entry.Params.Model, rates
			}
			return entry.Params.Model, [2]*float64{}
		}
	}
	_, rates, _ := tokenPrice(model, nil, nil)
	return model, rates
}

func pricedTokens(tokens int, price *float64) (float64, bool) {
	if tokens == 0 {
		return 0, true
	}
	if price == nil {
		return 0, false
	}
	cost := float64(tokens) * *price
	return cost, !math.IsInf(cost, 0)
}

func (registry *modelRegistry) estimateCost(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input costInput
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid cost estimate request")
		return
	}
	model := input.Model
	if model == "" {
		model = input.ModelName
	}
	in, out := input.counts()
	if model == "" || in < 0 || out < 0 ||
		(input.RequestsPerDay != nil && *input.RequestsPerDay < 0) ||
		(input.RequestsPerMonth != nil && *input.RequestsPerMonth < 0) {
		modelError(w, http.StatusBadRequest, "A model and non-negative token and request counts are required")
		return
	}
	resolved, rates := registry.costPrices(model)
	inputCost, hasInput := pricedTokens(in, rates[0])
	outputCost, hasOutput := pricedTokens(out, rates[1])
	total := inputCost + outputCost
	if (!hasInput || !hasOutput) || math.IsInf(total, 0) || (rates[0] == nil && rates[1] == nil) {
		modelError(w, http.StatusNotFound, "Model pricing is unavailable")
		return
	}
	provider, _, _ := deploymentIdentity(resolved, "")
	var margin float64
	if registry.base.CostAdjustment != nil {
		total, margin = registry.base.CostAdjustment.Price(provider, total)
	}
	response := map[string]any{
		"model": model, "input_tokens": in, "output_tokens": out,
		"cost_per_request": total, "input_cost_per_request": inputCost, "output_cost_per_request": outputCost,
		"margin_cost_per_request": margin, "input_cost_per_token": rates[0], "output_cost_per_token": rates[1],
		"num_requests_per_day": input.RequestsPerDay, "num_requests_per_month": input.RequestsPerMonth,
		"provider": nil,
	}
	if provider, _, found := strings.Cut(resolved, "/"); found {
		response["provider"] = provider
	} else if strings.HasPrefix(resolved, "gpt-") {
		response["provider"] = "openai"
	}
	for period, requests := range map[string]*int{"daily": input.RequestsPerDay, "monthly": input.RequestsPerMonth} {
		for suffix, cost := range map[string]float64{"_cost": total, "_input_cost": inputCost, "_output_cost": outputCost, "_margin_cost": margin} {
			response[period+suffix] = nil
			if requests != nil {
				periodCost := cost * float64(*requests)
				if math.IsInf(periodCost, 0) {
					modelError(w, http.StatusBadRequest, "Estimated cost is too large")
					return
				}
				response[period+suffix] = periodCost
			}
		}
	}
	if input.PromptTokens != nil || input.CompletionTokens != nil || input.ModelName != "" {
		response["cost"], response["currency"] = total, "USD"
		response["prompt_tokens"], response["completion_tokens"] = in, out
	}
	modelJSON(w, http.StatusOK, response)
}

func (registry *modelRegistry) calculateSpend(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		costInput
		Messages []map[string]any `json:"messages"`
		Completion json.RawMessage `json:"completion_response"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid spend calculation request")
		return
	}
	model := input.Model
	if model == "" {
		model = input.ModelName
	}
	in, out := input.counts()
	if model == "" && len(input.Completion) != 0 {
		var completion struct {
			Model string `json:"model"`
			Usage *struct {
				Input int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(input.Completion, &completion) != nil || completion.Usage == nil {
			modelError(w, http.StatusBadRequest, "Invalid completion response or missing usage")
			return
		}
		model = completion.Model
		in, out = completion.Usage.Input, completion.Usage.Output
	} else if model != "" && input.Messages == nil && input.InputTokens == nil &&
		input.OutputTokens == nil && input.PromptTokens == nil && input.CompletionTokens == nil {
		modelError(w, http.StatusBadRequest, "messages must be provided when model is provided")
		return
	}
	if model == "" || in < 0 || out < 0 {
		modelError(w, http.StatusBadRequest, "A model and valid usage are required")
		return
	}
	resolved, rates := registry.costPrices(model)
	if input.Messages != nil && (input.Model != "" || input.ModelName != "") {
		var err error
		in, err = runtime.CountChatInputTokens(resolved, input.Messages)
		if err != nil {
			modelError(w, http.StatusBadRequest, "Could not count message tokens")
			return
		}
		out = 0
	}
	inputCost, hasInput := pricedTokens(in, rates[0])
	outputCost, hasOutput := pricedTokens(out, rates[1])
	if !hasInput || !hasOutput || math.IsInf(inputCost+outputCost, 0) || (rates[0] == nil && rates[1] == nil) {
		modelError(w, http.StatusNotFound, "Model pricing is unavailable")
		return
	}
	total := inputCost + outputCost
	if policy := registry.base.CostAdjustment; policy != nil {
		provider, _, _ := deploymentIdentity(resolved, "")
		total, _ = policy.Price(provider, total)
	}
	modelJSON(w, http.StatusOK, map[string]float64{"cost": total})
}
