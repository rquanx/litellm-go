package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) evaluateOpenAIModeration(r *http.Request, guard GenericGuardrail, input map[string]any) *guardrailFailure {
	prompt := latestGuardrailUserPrompt(input)
	if prompt == "" {
		texts, _ := guardrailText(input)
		prompt = strings.Join(texts, "\n")
	}
	if prompt == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"model": guard.ModerationModel, "input": prompt})
	raw, failure := s.postGuardrail(r, guard, body, "Authorization", "Bearer "+guard.APIKey)
	if failure != nil || raw == nil {
		return failure
	}
	var verdict struct {
		Results []struct {
			Flagged *bool `json:"flagged"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &verdict) != nil || verdict.Results == nil || (len(verdict.Results) > 0 && verdict.Results[0].Flagged == nil) {
		return &guardrailFailure{502, "guardrail_error", "Invalid OpenAI Moderation response"}
	}
	if len(verdict.Results) > 0 && *verdict.Results[0].Flagged {
		return &guardrailFailure{400, "content_policy_violation", "Violated OpenAI moderation policy"}
	}
	return nil
}
