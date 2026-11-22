package runtime

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tiktoken-go/tokenizer"
)

func (s *Server) countTokens(w http.ResponseWriter, request *http.Request, scope []string) {
	var input struct {
		Model    string           `json:"model"`
		Prompt   *string          `json:"prompt"`
		Messages []map[string]any `json:"messages"`
		Contents []any            `json:"contents"`
		Tools    []any            `json:"tools"`
		System   any              `json:"system"`
	}
	if err := decodeBody(request, &input, s.config.MaxBodyBytes); err != nil || input.Model == "" ||
		(input.Prompt == nil && input.Messages == nil && input.Contents == nil) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A model and prompt, messages or contents are required")
		return
	}
	if !modelAllowed(scope, input.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	s.mu.RLock()
	route := s.configuredRoutes[input.Model]
	s.mu.RUnlock()
	model := input.Model
	if len(route.Candidates) > 0 {
		model = route.Candidates[0].ModelID
	}
	if request.URL.Query().Get("call_endpoint") == "true" && len(route.Candidates) > 0 {
		payload := map[string]any{"messages": input.Messages}
		if input.System != nil {
			payload["system"] = input.System
		}
		if input.Tools != nil {
			payload["tools"] = input.Tools
		}
		if len(input.Messages) > 0 {
			for _, candidate := range route.Candidates {
				if candidate.Endpoint != "anthropic-messages" || !candidateAllowed(scope, candidate) {
					continue
				}
				status, body, _, _, err := s.callJSON(request.Context(), request, "anthropic-count-tokens", candidate,
					requestPayload{fields: payload})
				var result struct {
					Count *int `json:"input_tokens"`
				}
				if err == nil && status >= 200 && status < 300 && json.Unmarshal(body, &result) == nil && result.Count != nil {
					writeJSON(w, http.StatusOK, tokenCountResult(*result.Count, input.Model, candidate.ModelID, candidate.Provider))
					return
				}
			}
		}
		if len(input.Contents) > 0 {
			for _, candidate := range route.Candidates {
				if !isGeminiEndpoint(candidate.Endpoint) || !candidateAllowed(scope, candidate) {
					continue
				}
				status, body, _, _, err := s.callJSON(request.Context(), request, "gemini-count-tokens", candidate,
					requestPayload{fields: map[string]any{"contents": input.Contents}})
				var result struct {
					Count *int `json:"totalTokens"`
				}
				if err == nil && status >= 200 && status < 300 && json.Unmarshal(body, &result) == nil && result.Count != nil {
					writeJSON(w, http.StatusOK, tokenCountResult(*result.Count, input.Model, candidate.ModelID, candidate.Provider))
					return
				}
			}
		}
	}
	codec, err := tokenizerForModel(model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token_counting_error", "Could not select a tokenizer")
		return
	}
	total := 0
	switch {
	case input.Prompt != nil:
		total, err = codec.Count(*input.Prompt)
	case input.Messages != nil:
		total, err = countChatTokens(codec, input.Messages)
	default:
		var encoded []byte
		encoded, err = json.Marshal(input.Contents)
		if err == nil {
			total, err = codec.Count(string(encoded))
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "token_counting_error", "Could not tokenize the request")
		return
	}
	writeJSON(w, http.StatusOK, tokenCountResult(total, input.Model, model, "openai_tokenizer"))
}

func tokenCountResult(total int, requested, used, backend string) map[string]any {
	return map[string]any{"total_tokens": total, "request_model": requested, "model_used": used, "tokenizer_type": backend}
}

func tokenizerForModel(model string) (tokenizer.Codec, error) {
	for _, prefix := range []string{"openai/", "azure/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	if strings.HasPrefix(model, "gpt-5") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") {
		return tokenizer.Get(tokenizer.O200kBase)
	}
	codec, err := tokenizer.ForModel(tokenizer.Model(model))
	if err == nil {
		return codec, nil
	}
	return tokenizer.Get(tokenizer.Cl100kBase)
}

func countChatTokens(codec tokenizer.Codec, messages []map[string]any) (int, error) {
	total := 3
	for _, message := range messages {
		total += 3
		for _, field := range []string{"role", "content", "name", "tool_calls", "function_call"} {
			value, exists := message[field]
			if !exists || value == nil {
				continue
			}
			asText, ok := value.(string)
			if !ok {
				raw, err := json.Marshal(value)
				if err != nil {
					return 0, err
				}
				asText = string(raw)
			}
			count, err := codec.Count(asText)
			if err != nil {
				return 0, err
			}
			total += count
			if field == "name" {
				total++
			}
		}
	}
	return total, nil
}
