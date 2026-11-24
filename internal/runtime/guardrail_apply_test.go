package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardrailApplyChecksVisibilityAndReturnsProcessedText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Stage string `json:"input_type"`
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Stage != "response" ||
			len(input.Texts) != 1 || input.Texts[0] != "fixture" {
			t.Errorf("unexpected guardrail input: %+v (%v)", input, err)
		}
		w.Write([]byte(`{"action":"GUARDRAIL_INTERVENED","texts":["processed"]}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true, HTTPClient: upstream.Client(),
		AuthorizeKey: func(token string) ([]string, bool) { return nil, token == "team" || token == "other" },
		GuardrailTeam: func(r *http.Request) string { return ProxyCredential(r) },
		Guardrails: []GenericGuardrail{{Name: "fixture", URL: upstream.URL, Team: "team", Post: true}}})
	for token, status := range map[string]int{"team": 200, "other": 404, "bad": 401} {
		request := httptest.NewRequest(http.MethodPost, "/guardrails/apply_guardrail",
			strings.NewReader(`{"guardrail_name":"fixture","text":"fixture","messages":[{"role":"user","content":"context"}]}`))
		request.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		server.ServeHTTP(result, request)
		if result.Code != status || (status == http.StatusOK && !strings.Contains(result.Body.String(), `"response_text":"processed"`)) {
			t.Fatalf("unexpected response: %d %s", result.Code, result.Body)
		}
	}
}

