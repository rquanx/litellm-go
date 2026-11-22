package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetrySameDeploymentBeforeFallback(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"provider","choices":[]}`)
	}))
	defer upstream.Close()
	server := NewServer(Config{NumRetries: 1, Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{ModelID: "provider", APIBase: upstream.URL, Endpoint: "chat-completions"},
	}}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || result.Header().Get("X-Minato-Runtime-Attempts") != "2" || calls.Load() != 2 {
		t.Fatalf("unexpected result after retry: %d %s", result.Code, result.Body.String())
	}
}
