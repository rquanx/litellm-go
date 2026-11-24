package runtime

import (
	"net/http"
	"slices"
)

func (s *Server) servePolicyGuardrailTest(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) { writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required"); return }
	var request struct { GuardrailNames []string `json:"guardrail_names"`; InputsList []map[string]any `json:"inputs_list"`; InputType string `json:"input_type"` }
	if decodeBody(r, &request, s.config.MaxBodyBytes) != nil { writeError(w, http.StatusBadRequest, "invalid_request", "Invalid policy test request"); return }
	guards := s.visibleGuardrails(r)
	selected := guards
	if len(request.GuardrailNames) > 0 { selected = slices.DeleteFunc(append([]GenericGuardrail{}, guards...), func(guard GenericGuardrail) bool { return !slices.Contains(request.GuardrailNames, guard.Name) }) }
	results := make([]map[string]any, 0, len(request.InputsList))
	for _, item := range request.InputsList {
		text := ""
		if value, ok := item["input"].(string); ok { text = value }; if text == "" { if value, ok := item["text"].(string); ok { text = value } }
		input := map[string]any{"input": text}; errorsFound := []map[string]any{}
		for _, guard := range selected { if failure := s.guardrailVerdict(r, guard, "", "request", input); failure != nil { errorsFound = append(errorsFound, map[string]any{"guardrail_name": guard.Name, "message": "Guardrail blocked input"}); break } }
		results = append(results, map[string]any{"inputs": input, "guardrail_errors": errorsFound})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) serveGuardrailApply(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		if s.config.AuthorizeKey == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return
		}
		if _, ok := s.config.AuthorizeKey(ProxyCredential(r)); !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return
		}
	}
	var request struct {
		Name string `json:"guardrail_name"`
		Text *string `json:"text"`
		Stage string `json:"input_type"`
		Messages []map[string]any `json:"messages"`
	}
	if decodeBody(r, &request, s.config.MaxBodyBytes) != nil || request.Name == "" || request.Text == nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_request", "Supply a guardrail name and text")
		return
	}
	guards := s.visibleGuardrails(r)
	index := slices.IndexFunc(guards, func(guard GenericGuardrail) bool { return guard.Name == request.Name })
	if index < 0 {
		writeError(w, http.StatusNotFound, "guardrail_not_found", "Guardrail not found")
		return
	}
	guard := guards[index]
	stage := "request"
	if request.Stage == "response" || ((request.Stage == "" || request.Stage == "request") && guard.Post) {
		stage = "response"
	}
	input := map[string]any{"input": *request.Text}
	if len(request.Messages) > 0 {
		input["guardrail_context_messages"] = request.Messages
	}
	if !s.checkGenericGuardrail(w, r, guard, "", stage, input) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"response_text": input["input"]})
}

