package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDotpromptTestRendersSystemAndStreamsThroughProxy(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"dotprompt_content":    "---\nmodel: fixture\ntemperature: 0.2\n---\nSystem: Reply as {{user.name}}\nUser: ignored",
		"prompt_variables":     map[string]any{"user": map[string]any{"name": "Ada"}},
		"conversation_history": []map[string]any{{"role": "user", "content": "hello"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/prompts/test", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder()
	api := promptAPI{keys: keys}
	api.testPrompt(w, r, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer master" ||
			json.NewDecoder(request.Body).Decode(&payload) != nil {
			t.Fatal("prompt test did not use the configured inference handler")
		}
		got, _ := json.Marshal(payload)
		want := `{"messages":[{"content":"Reply as Ada","role":"system"},{"content":"hello","role":"user"}],"model":"fixture","stream":true,"temperature":0.2}`
		if string(got) != want {
			t.Fatalf("unexpected rendered payload %s", got)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		response.Write([]byte("data: [DONE]\n\n"))
	}))
	if w.Code != http.StatusOK || w.Body.String() != "data: [DONE]\n\n" {
		t.Fatalf("unexpected prompt test result %d %s", w.Code, w.Body)
	}
}
