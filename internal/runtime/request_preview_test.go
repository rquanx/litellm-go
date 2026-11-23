package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransformRequestIsScopedAndNeverCallsTheProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("preview contacted the upstream")
	}))
	defer upstream.Close()
	server := NewServer(Config{
		Token: "master", AuthorizeInference: true,
		AuthorizeKey: func(value string) ([]string, bool) { return []string{"other"}, value == "restricted" },
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "anthropic", Endpoint: "anthropic-messages",
			ModelID: "claude-fixture", APIBase: upstream.URL, APIKey: "fixture-secret"}}}},
	})
	for _, test := range []struct {
		key    string
		status int
	}{{"", 401}, {"restricted", 403}, {"master", 200}} {
		r := httptest.NewRequest("POST", "/utils/transform_request", strings.NewReader(`{"call_type":"completion","request_body":{"model":"demo","api_key":"request-secret","messages":[{"role":"user","content":"fixture"}]}}`))
		r.Header.Set("Authorization", "Bearer "+test.key)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		value := w.Body.String()
		if w.Code != test.status || strings.Contains(value, "secret") {
			t.Fatalf("preview status %d: %s", w.Code, value)
		}
		if w.Code == 200 && (!strings.Contains(value, "/v1/messages") || !strings.Contains(value, "claude-fixture")) {
			t.Fatalf("invalid preview: %s", value)
		}
	}
}
