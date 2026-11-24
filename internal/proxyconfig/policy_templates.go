package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed policy_templates.json
var policyTemplatesJSON []byte

func servePolicyTemplates(w http.ResponseWriter, _ *http.Request) {
	var templates []map[string]any
	if json.Unmarshal(policyTemplatesJSON, &templates) != nil {
		modelError(w, http.StatusInternalServerError, "Policy templates are unavailable")
		return
	}
	modelJSON(w, http.StatusOK, templates)
}

func servePolicyTemplateEnrich(w http.ResponseWriter, r *http.Request) {
	var input struct { TemplateID string `json:"template_id"`; Parameters map[string]any `json:"parameters"`; Competitors []string `json:"competitors"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.TemplateID == "" {
		modelError(w, http.StatusBadRequest, "template_id and parameters are required"); return
	}
	var templates []map[string]any
	if json.Unmarshal(policyTemplatesJSON, &templates) != nil { modelError(w, http.StatusInternalServerError, "Policy templates are unavailable"); return }
	var template map[string]any
	for _, candidate := range templates { if candidate["id"] == input.TemplateID { template = candidate; break } }
	if template == nil { modelError(w, http.StatusNotFound, "Template not found: "+input.TemplateID); return }
	if len(input.Competitors) == 0 { modelError(w, http.StatusBadRequest, "competitors are required for local enrichment"); return }
	values := map[string]string{
		"{{competitors_blocked_words}}": strings.Join(input.Competitors, "\n"),
		"{{competitor_recommendation_words}}": "recommend\nsuggest\ntry\nswitch",
		"{{competitor_comparison_words}}": "better than\nversus\ncompare\ncomparison",
	}
	enriched := deepReplaceTemplateValues(template["guardrailDefinitions"], values)
	result := map[string]any{"guardrailDefinitions": enriched, "competitors": input.Competitors, "competitor_variations": map[string][]string{}}
	if strings.HasSuffix(r.URL.Path, "/stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		body, _ := json.Marshal(result)
		_, _ = w.Write(append(append([]byte("data: "), body...), []byte("\n\n")...))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func deepReplaceTemplateValues(value any, replacements map[string]string) any {
	switch current := value.(type) {
	case string:
		for from, to := range replacements { current = strings.ReplaceAll(current, from, to) }
		return current
	case []any:
		result := make([]any, len(current)); for i, item := range current { result[i] = deepReplaceTemplateValues(item, replacements) }; return result
	case map[string]any:
		result := make(map[string]any, len(current)); for key, item := range current { result[key] = deepReplaceTemplateValues(item, replacements) }; return result
	default:
		return value
	}
}

func servePolicyTemplateTest(w http.ResponseWriter, r *http.Request) {
	var input struct { Definitions []map[string]any `json:"guardrail_definitions"`; Text string `json:"text"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.Text == "" { modelError(w, http.StatusBadRequest, "guardrail_definitions and text are required"); return }
	results := make([]map[string]any, 0, len(input.Definitions)); overall := "passed"
	for _, definition := range input.Definitions {
		name, _ := definition["guardrail_name"].(string); params, _ := definition["litellm_params"].(map[string]any); blocked, _ := params["blocked_words"].(string)
		action, output := "passed", input.Text
		for _, word := range strings.Split(blocked, "\n") { word = strings.TrimSpace(word); if word != "" && strings.Contains(strings.ToLower(input.Text), strings.ToLower(word)) { action, output, overall = "blocked", "", "blocked"; break } }
		results = append(results, map[string]any{"guardrail_name": name, "action": action, "output_text": output, "details": "Local blocked-word evaluation"})
	}
	modelJSON(w, http.StatusOK, map[string]any{"overall_action": overall, "results": results})
}

func servePolicyTemplateSuggest(w http.ResponseWriter, r *http.Request) {
	var input struct { AttackExamples []string `json:"attack_examples"`; Description string `json:"description"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil { modelError(w, http.StatusBadRequest, "Invalid template suggestion request"); return }
	var templates []map[string]any; if json.Unmarshal(policyTemplatesJSON, &templates) != nil { modelError(w, http.StatusInternalServerError, "Policy templates are unavailable"); return }
	query := strings.ToLower(input.Description + " " + strings.Join(input.AttackExamples, " ")); matches := make([]map[string]any, 0)
	for _, template := range templates { title, _ := template["title"].(string); description, _ := template["description"].(string); if query == "" || strings.Contains(strings.ToLower(title), query) || strings.Contains(strings.ToLower(description), query) { matches = append(matches, map[string]any{"template_id": template["id"], "title": title, "reason": "Matched local template metadata"}) } }
	modelJSON(w, http.StatusOK, map[string]any{"suggestions": matches})
}
