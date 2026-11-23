package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type GenericGuardrail struct {
	Name, URL, APIKey       string
	Default, FailOpen, Post bool
	Parameters              map[string]any
	Provider                string
	Azure                   *AzureSafetyOptions
	ModerationModel         string
}

type GuardrailPolicy struct {
	Guardrails []string
	Matches    []GuardrailPolicyMatch
	Pipelines  []PolicyPipeline
	Error      string
	Status     int
}

type responseGuardrails struct {
	independent []GenericGuardrail
	pipelines   []PolicyPipeline
	available   []GenericGuardrail
}

func (plan responseGuardrails) active() bool {
	return len(plan.independent) > 0 || len(plan.pipelines) > 0
}

type GuardrailPolicyMatch struct {
	Name   string
	Reason string
}

func (s *Server) ReplaceGuardrails(guards []GenericGuardrail) {
	s.mu.Lock()
	s.guardrails = guards
	s.mu.Unlock()
}

func (s *Server) applyGenericGuardrails(w http.ResponseWriter, r *http.Request, model, endpoint string, input map[string]any, stream bool) (responseGuardrails, bool) {
	s.mu.RLock()
	guards := s.guardrails
	s.mu.RUnlock()
	if len(guards) == 0 && s.config.ResolvePolicy == nil {
		delete(input, "policies")
		return responseGuardrails{}, true
	}
	requested := make(map[string]bool)
	if value := input["guardrails"]; value != nil {
		items, ok := value.([]any)
		if !ok {
			writeError(w, 400, "guardrail_error", "Expected a list of guardrail names")
			return responseGuardrails{}, false
		}
		for _, item := range items {
			name, ok := item.(string)
			if !ok || name == "" {
				writeError(w, 400, "guardrail_error", "Expected a guardrail name")
				return responseGuardrails{}, false
			}
			requested[name] = true
		}
	}
	delete(input, "guardrails")
	var policy GuardrailPolicy
	if s.config.ResolvePolicy != nil {
		policy = s.config.ResolvePolicy(r, model, input)
		if policy.Error != "" {
			writeError(w, policy.Status, "guardrail_error", policy.Error)
			return responseGuardrails{}, false
		}
		for _, name := range policy.Guardrails {
			requested[name] = true
		}
		writePolicyAttribution(w.Header(), policy.Matches)
	}
	delete(input, "policies")
	plan := responseGuardrails{available: guards}
	managed := make(map[string]bool)
	for _, pipeline := range policy.Pipelines {
		for _, step := range pipeline.Spec.Steps {
			managed[step.Guardrail] = true
			delete(requested, step.Guardrail)
		}
		if pipeline.Spec.Mode == "post_call" {
			plan.pipelines = append(plan.pipelines, pipeline)
		}
	}
	var selected []GenericGuardrail
	for _, guard := range guards {
		if managed[guard.Name] {
			continue
		}
		if guard.Default || requested[guard.Name] {
			selected = append(selected, guard)
			delete(requested, guard.Name)
		}
	}
	if len(requested) != 0 {
		writeError(w, 400, "guardrail_error", "Unknown guardrail")
		return responseGuardrails{}, false
	}
	for _, guard := range selected {
		if guard.Post {
			plan.independent = append(plan.independent, guard)
		}
	}
	if stream && plan.active() {
		writeError(w, 400, "guardrail_error", "Streaming requires a compatible response guardrail")
		return responseGuardrails{}, false
	}
	for _, pipeline := range policy.Pipelines {
		if pipeline.Spec.Mode == "pre_call" && !s.enforcePipeline(w, r, pipeline, guards, model, endpoint, stream, input) {
			return responseGuardrails{}, false
		}
	}
	for _, guard := range selected {
		if guard.Post {
			continue
		}
		if !s.checkGenericGuardrail(w, r, guard, model, "request", input) {
			return responseGuardrails{}, false
		}
	}
	return plan, true
}

func writePolicyAttribution(headers http.Header, matches []GuardrailPolicyMatch) {
	var names, sources []string
	for _, match := range matches {
		if strings.ContainsAny(match.Name+match.Reason, "\r\n\x00") {
			continue
		}
		names = append(names, match.Name)
		sources = append(sources, match.Name+"="+match.Reason)
	}
	if len(names) > 0 {
		headers.Set("X-LiteLLM-Applied-Policies", strings.Join(names, ","))
		headers.Set("X-LiteLLM-Policy-Sources", strings.Join(sources, "; "))
	}
}

func (s *Server) checkGenericGuardrail(w http.ResponseWriter, r *http.Request, guard GenericGuardrail, model, stage string, input map[string]any) bool {
	failure := s.evaluateGenericGuardrail(r, guard, model, stage, input)
	if failure != nil {
		writeError(w, failure.status, failure.kind, failure.message)
		return false
	}
	return true
}

type guardrailFailure struct {
	status  int
	kind    string
	message string
}

func (s *Server) evaluateGenericGuardrail(r *http.Request, guard GenericGuardrail, model, stage string, input map[string]any) *guardrailFailure {
	if guard.Azure != nil {
		return s.evaluateAzureSafety(r, guard, stage, input)
	}
	if guard.Provider == "openai_moderation" {
		return s.evaluateOpenAIModeration(r, guard, input)
	}
	texts, replace := guardrailText(input)
	body, _ := json.Marshal(map[string]any{"model": model, "input_type": stage, "texts": texts,
		"structured_messages": input["messages"], "tools": input["tools"], "request_data": map[string]any{},
		"litellm_version": "1.85.0", "additional_provider_specific_params": guard.Parameters})
	var verdict struct {
		Action string   `json:"action"`
		Texts  []string `json:"texts"`
		Reason string   `json:"blocked_reason"`
	}
	raw, failure := s.postGuardrail(r, guard, body, "Authorization", "Bearer "+guard.APIKey)
	if failure != nil || raw == nil {
		return failure
	}
	if json.Unmarshal(raw, &verdict) != nil {
		return &guardrailFailure{502, "guardrail_error", "Invalid guardrail response"}
	}
	switch verdict.Action {
	case "BLOCKED":
		if verdict.Reason == "" {
			verdict.Reason = "Guardrail blocked the request"
		}
		return &guardrailFailure{400, "content_policy_violation", verdict.Reason}
	case "", "NONE":
		return nil
	case "GUARDRAIL_INTERVENED":
		if len(verdict.Texts) == len(texts) {
			replace(verdict.Texts)
			return nil
		}
	}
	return &guardrailFailure{502, "guardrail_error", "Unexpected guardrail action or text count"}
}

func (s *Server) postGuardrail(r *http.Request, guard GenericGuardrail, body []byte, header, credential string) ([]byte, *guardrailFailure) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, guard.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &guardrailFailure{502, "guardrail_error", "Invalid guardrail URL"}
	}
	req.Header.Set("Content-Type", "application/json")
	if guard.APIKey != "" {
		req.Header.Set(header, credential)
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, &guardrailFailure{503, "guardrail_error", "Guardrail request timed out"}
	}
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if guard.FailOpen && r.Context().Err() == nil {
			return nil, nil
		}
		return nil, &guardrailFailure{502, "guardrail_error", "Could not reach guardrail"}
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		if guard.FailOpen && response.StatusCode >= 502 && response.StatusCode <= 504 {
			return nil, nil
		}
		return nil, &guardrailFailure{502, "guardrail_error", "Guardrail service rejected the request"}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, &guardrailFailure{502, "guardrail_error", "Invalid guardrail response"}
	}
	return raw, nil
}

func guardrailText(input map[string]any) ([]string, func([]string)) {
	texts := make([]string, 0)
	var targets []struct {
		object map[string]any
		field  string
	}
	add := func(object map[string]any, field string) {
		if text, ok := object[field].(string); ok {
			texts = append(texts, text)
			targets = append(targets, struct {
				object map[string]any
				field  string
			}{object, field})
		}
	}
	add(input, "input")
	for _, key := range []string{"messages", "input", "output", "choices"} {
		items, _ := input[key].([]any)
		for _, raw := range items {
			message, _ := raw.(map[string]any)
			if nested, ok := message["message"].(map[string]any); ok {
				message = nested
			}
			add(message, "text")
			add(message, "content")
			parts, _ := message["content"].([]any)
			for _, raw := range parts {
				part, _ := raw.(map[string]any)
				add(part, "text")
			}
		}
	}
	return texts, func(values []string) {
		for i, target := range targets {
			target.object[target.field] = values[i]
		}
	}
}

func (s *Server) applyResponseGuardrails(w http.ResponseWriter, r *http.Request, model, endpoint string, plan responseGuardrails, body []byte) ([]byte, bool) {
	var fields map[string]any
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		writeError(w, http.StatusBadGateway, "guardrail_error", "Could not read inference response")
		return nil, false
	}
	for _, pipeline := range plan.pipelines {
		if !s.enforcePipeline(w, r, pipeline, plan.available, model, endpoint, false, fields) {
			return nil, false
		}
	}
	for _, guard := range plan.independent {
		if !s.checkGenericGuardrail(w, r, guard, model, "response", fields) {
			return nil, false
		}
	}
	result, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusBadGateway, "guardrail_error", "Could not encode checked response")
		return nil, false
	}
	return result, true
}
