package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestBlockedWordsFileValidation(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "", keys)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, token, content string
		status int
		message string
		errors []string
	}{
		{name: "unauthenticated", status: 401},
		{name: "valid", token: "master", content: "blocked_words: [{keyword: example, action: MASK}]", status: 200, message: "Valid YAML file with 1 blocked word(s)"},
		{name: "bad entries", token: "master", content: "blocked_words: [example, {keyword: 23, action: allow, description: false}, {}]", status: 200,
			errors: []string{"Entry 0: must be an object", "Entry 1: 'keyword' must be a string", "Entry 1: action must be 'BLOCK' or 'MASK', got 'allow'",
				"Entry 1: 'description' must be a string", "Entry 2: missing 'keyword' field", "Entry 2: missing 'action' field"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"file_content": test.content})
			request := httptest.NewRequest(http.MethodPost, "/guardrails/validate_blocked_words_file", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("got %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 {
				return
			}
			var result struct {
				Valid bool `json:"valid"`
				Message string `json:"message"`
				Errors []string `json:"errors"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Valid != (test.errors == nil) ||
				result.Message != test.message || !reflect.DeepEqual(result.Errors, test.errors) {
				t.Fatalf("unexpected validation response: %s", response.Body.String())
			}
		})
	}
}
