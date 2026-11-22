package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestModelRetryPolicyOverridesGlobalRetryLimit(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
			return
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, Endpoint: "chat-completions"}}}},
		RetryPolicy: RetryPolicy{"AuthenticationErrorRetries": 0},
		ModelRetryPolicies: map[string]RetryPolicy{"demo": {"AuthenticationErrorRetries": 1}},
	})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || calls.Load() != 2 {
		t.Fatalf("configured retry did not run: %d %s", response.Code, response.Body)
	}
}
