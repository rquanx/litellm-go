package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInferenceCacheControlsScopeAndSpend(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requests.Add(1)
		body, err := readBody(r, 4096)
		if err != nil || strings.Contains(string(body), `"cache"`) || strings.Contains(string(body), `"caching"`) {
			t.Errorf("cache directives reached provider: %s, %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"request-%d","usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, count)
	}))
	defer upstream.Close()
	price := 1.0
	candidate := Candidate{APIBase: upstream.URL + "/v1", ModelID: "native", SourceID: "fixture",
		Endpoint: "chat-completions", UsagePrice: [2]*float64{&price, &price}}
	var event InferenceEvent
	server := NewServer(Config{
		Token: "master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) {
			return []string{"demo"}, token == "key-one" || token == "key-two"
		},
		Routes:         []Route{{Alias: "demo", Candidates: []Candidate{candidate}}},
		InferenceCache: InferenceCacheConfig{Enabled: true, TTL: time.Minute, Endpoints: map[string]bool{"chat-completions": true}},
		Callback:       func(value InferenceEvent) { event = value },
	})
	call := func(key, control, extras string, expectedRequests int32, hit bool) string {
		t.Helper()
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"demo","messages":[{"role":"user","content":"hello"}]`+extras+"}"))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Cache-Control", control)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 || requests.Load() != expectedRequests || event.CacheHit != hit || event.Total != 3 ||
			event.Spend == nil || (hit && *event.Spend != 0) || (!hit && *event.Spend != 3) {
			t.Fatalf("requests %d, response %d %s, event %+v", requests.Load(), response.Code, response.Body, event)
		}
		return response.Header().Get("X-LiteLLM-Cache-Key")
	}
	if call("key-one", "", "", 1, false) != "" {
		t.Fatal("cache key should be present only on hits")
	}
	first := call("key-one", "", "", 1, true)
	if first == "" || first != call("key-one", "", "", 1, true) {
		t.Fatal("expected one stable cache key")
	}
	call("key-two", "", "", 2, false)
	if first == call("key-two", "", "", 2, true) {
		t.Fatal("cache key leaked across virtual keys")
	}
	call("key-one", "no-cache, no-store", "", 3, false)
	call("key-one", "", "", 3, true)
	call("key-one", "", `,"caching":false`, 4, false)
	call("key-one", "no-cache", `,"cache":{"ttl":0.005}`, 5, false)
	time.Sleep(20 * time.Millisecond)
	call("key-one", "", "", 6, false)
	candidate.APIKey = "rotated"
	server.ReplaceConfiguredRouting([]Route{{Alias: "demo", Candidates: []Candidate{candidate}}}, nil, nil)
	call("key-one", "", "", 7, false)
}
