package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiInteractionLifecycleAndStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "fixture" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect Google credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1beta/interactions":
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || (body["model"] != "gemini-test" && body["agent"] != "gemini-test") ||
				body["custom_llm_provider"] != nil {
				t.Errorf("incorrect Google payload: %v", body)
			}
			if body["stream"] == true {
				if r.URL.Query().Get("alt") != "sse" || body["previous_interaction_id"] != "interaction-fixture" {
					t.Errorf("incorrect streaming request: %v %v", r.URL.Query(), body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: interaction.complete\ndata: {\"interaction\":{\"id\":\"interaction-fixture\",\"usage\":{\"total_input_tokens\":2,\"total_output_tokens\":3,\"total_tokens\":5}}}\n\n")
				return
			}
			fmt.Fprint(w, `{"id":"interaction-fixture","status":"completed","usage":{"total_input_tokens":2,"total_output_tokens":3,"total_tokens":5}}`)
		case "GET /v1beta/interactions/interaction-fixture", "POST /v1beta/interactions/interaction-fixture:cancel":
			fmt.Fprint(w, `{"id":"interaction-fixture","status":"completed"}`)
		case "DELETE /v1beta/interactions/interaction-fixture":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Google operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	var event InferenceEvent
	server := NewServer(Config{
		AuthorizeInference: true, Token: "master",
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"other"}, key == "restricted" },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL + "/v1beta", APIKey: "fixture",
			ModelID: "gemini-test", Provider: "gemini", Endpoint: "gemini-generate", DeploymentID: "deployment"}}}},
		Callback: func(result InferenceEvent) { event = result },
	})
	call := func(method, path, input, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(input))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w
	}
	id := encodeResourceID(interactionFamily, "deployment", "interaction-fixture")
	if result := call("POST", "/interactions", `{"model":"demo","input":"hello"}`, "master", 200); !strings.Contains(result.Body.String(), id) ||
		event.Prompt != 2 || event.Completion != 3 || event.Total != 5 {
		t.Fatalf("unexpected interaction result %s or usage %+v", result.Body, event)
	}
	result := call("POST", interactionPath, `{"agent":"demo","stream":true,"previous_interaction_id":"`+id+`"}`, "master", 200)
	if !result.Flushed || !strings.Contains(result.Body.String(), id) || event.Total != 5 {
		t.Fatalf("unexpected interaction stream %s and usage %+v", result.Body, event)
	}
	call("GET", interactionPath+"/"+id, "", "restricted", 400)
	call("GET", interactionPath+"/"+id, "", "", 401)
	call("GET", interactionPath+"/"+id, "", "master", 200)
	call("POST", interactionPath+"/"+id+"/cancel", "", "master", 200)
	deleted := call("DELETE", interactionPath+"/"+id, "", "master", 200)
	if !strings.Contains(deleted.Body.String(), `"success":true`) || !strings.Contains(deleted.Body.String(), id) {
		t.Fatalf("unexpected delete result %s", deleted.Body)
	}
}
