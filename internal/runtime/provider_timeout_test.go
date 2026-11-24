package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeploymentRetryAndRequestTimeoutOverrides(t *testing.T) {
	var calls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := readBody(r, 4096)
		if strings.Contains(string(data), "timeout") {
			t.Errorf("timeout reached provider JSON: %s", data)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer fixture.Close()
	retries := 1
	candidate := Candidate{APIBase: fixture.URL, ModelID: "fixture", Endpoint: "chat-completions", NumRetries: &retries,
		Timeout: time.Microsecond, StreamTimeout: 2 * time.Second}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[],"timeout":2}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("deployment retry did not override the router default: %d %s, calls %d", response.Code, response.Body, calls.Load())
	}
	if got := providerRequestTimeout(map[string]any{"timeout":1.0}, candidate, true, time.Second); got != 2*time.Second {
		t.Fatalf("selected timeout %v", got)
	}
}
