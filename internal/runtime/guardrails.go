package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

type GenericGuardrail struct {
	Name, URL, APIKey       string
	Default, FailOpen, Post bool
	Parameters              map[string]any
}

func (s *Server) applyGenericGuardrails(w http.ResponseWriter, r *http.Request, model string, input map[string]any, stream bool) ([]GenericGuardrail, bool) {
	requested := make(map[string]bool)
	if value := input["guardrails"]; value != nil {
		items, ok := value.([]any)
		if !ok {
			writeError(w, 400, "guardrail_error", "Expected a list of guardrail names")
			return nil, false
		}
		for _, item := range items {
			name, ok := item.(string)
			if !ok || name == "" {
				writeError(w, 400, "guardrail_error", "Expected a guardrail name")
				return nil, false
			}
			requested[name] = true
		}
	}
	delete(input, "guardrails")
	var selected []GenericGuardrail
	for _, guard := range s.config.Guardrails {
		if guard.Default || requested[guard.Name] {
			selected = append(selected, guard)
			delete(requested, guard.Name)
		}
	}
	if len(requested) != 0 {
		writeError(w, 400, "guardrail_error", "Unknown guardrail")
		return nil, false
	}
	var post []GenericGuardrail
	for _, guard := range selected {
		if guard.Post {
			if stream {
				writeError(w, 400, "guardrail_error", "Streaming requires a compatible response guardrail")
				return nil, false
			}
			post = append(post, guard)
			continue
		}
		if !s.checkGenericGuardrail(w, r, guard, model, "request", input) {
			return nil, false
		}
	}
	return post, true
}

func (s *Server) checkGenericGuardrail(w http.ResponseWriter, r *http.Request, guard GenericGuardrail, model, stage string, input map[string]any) bool {
	texts, replace := guardrailText(input)
	body, _ := json.Marshal(map[string]any{"model": model, "input_type": stage, "texts": texts,
		"structured_messages": input["messages"], "tools": input["tools"], "request_data": map[string]any{},
		"litellm_version": "1.85.0", "additional_provider_specific_params": guard.Parameters})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, guard.URL, bytes.NewReader(body))
	if err != nil {
		writeError(w, 502, "guardrail_error", "Invalid guardrail URL")
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if guard.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+guard.APIKey)
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		writeError(w, 503, "guardrail_error", "Guardrail request timed out")
		return false
	}
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if guard.FailOpen && r.Context().Err() == nil {
			return true
		}
		writeError(w, 502, "guardrail_error", "Could not reach guardrail")
		return false
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		if guard.FailOpen && response.StatusCode >= 502 && response.StatusCode <= 504 {
			return true
		}
		writeError(w, 502, "guardrail_error", "Guardrail service rejected the request")
		return false
	}
	var verdict struct {
		Action string   `json:"action"`
		Texts  []string `json:"texts"`
		Reason string   `json:"blocked_reason"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || json.Unmarshal(raw, &verdict) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		writeError(w, 502, "guardrail_error", "Invalid guardrail response")
		return false
	}
	switch verdict.Action {
	case "BLOCKED":
		if verdict.Reason == "" {
			verdict.Reason = "Guardrail blocked the request"
		}
		writeError(w, 400, "content_policy_violation", verdict.Reason)
		return false
	case "", "NONE":
		return true
	case "GUARDRAIL_INTERVENED":
		if len(verdict.Texts) == len(texts) {
			replace(verdict.Texts)
			return true
		}
	}
	writeError(w, 502, "guardrail_error", "Unexpected guardrail action or text count")
	return false
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

func (s *Server) applyResponseGuardrails(w http.ResponseWriter, r *http.Request, model string, guards []GenericGuardrail, body []byte) ([]byte, bool) {
	var fields map[string]any
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		writeError(w, http.StatusBadGateway, "guardrail_error", "Could not read inference response")
		return nil, false
	}
	for _, guard := range guards {
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
