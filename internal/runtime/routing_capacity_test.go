package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyParallelLimitReleasesOnCompletion(t *testing.T) {
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	limit := 1
	server := NewServer(Config{Token: "master", RequireInferenceAuth: true,
		AuthorizeKey: func(key string) ([]string, bool) { return nil, key == "limited" },
		CheckKeyBudget: func(context.Context, string) (KeyBudget, error) {
			return KeyBudget{SpendIdentity: "limited", ParallelLimit: &limit}, nil
		},
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{
			{APIBase: upstream.URL, Endpoint: "chat-completions", ModelID: "fixture"},
		}}},
	})
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
		request.Header.Set("Authorization", "Bearer limited")
		result := httptest.NewRecorder()
		server.ServeHTTP(result, request)
		return result
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- call() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the first request did not reach the provider")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- call() }()
	select {
	case result := <-second:
		if result.Code != http.StatusTooManyRequests || !strings.Contains(result.Body.String(), "parallel request limit") {
			t.Fatalf("unexpected response to concurrent request: %d %s", result.Code, result.Body)
		}
	case <-entered:
		t.Fatal("the rejected request reached the provider")
	case <-time.After(3 * time.Second):
		t.Fatal("the concurrent request was not rejected")
	}
	close(release)
	if result := <-first; result.Code != http.StatusOK {
		t.Fatalf("first request failed: %d %s", result.Code, result.Body)
	}
	if result := call(); result.Code != http.StatusOK {
		t.Fatalf("the completed request kept its concurrency slot: %d %s", result.Code, result.Body)
	}
}

func TestCapacityRoutingFallsBackWithoutSendingAnOverLimitRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary/v1/chat/completions" {
			w.Write([]byte(`{"choices":[],"usage":{"total_tokens":17}}`))
			return
		}
		w.Write([]byte(`{"choices":[],"usage":{"total_tokens":4}}`))
	}))
	defer upstream.Close()
	tpm := 16
	primary := Candidate{SourceID: "primary", DeploymentID: "primary", RoutingGroup: "primary", Endpoint: "chat-completions",
		ModelID: "m", APIBase: upstream.URL + "/primary", TPM: &tpm}
	backup := Candidate{SourceID: "backup", DeploymentID: "backup", RoutingGroup: "backup", Endpoint: "chat-completions",
		ModelID: "m", APIBase: upstream.URL + "/backup"}
	server := NewServer(Config{RoutingStrategy: "usage-based-routing-v2",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{primary, backup}}}})
	for _, source := range []string{"primary", "backup"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("X-Minato-Runtime-Source") != source {
			t.Fatalf("expected %s, got %s (%d): %s", source, w.Header(), w.Code, w.Body)
		}
	}
}

func TestConfiguredCooldownSkipsFailedDeployment(t *testing.T) {
	var primaryCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/primary/") {
			primaryCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	primary := Candidate{DeploymentID: "primary", RoutingGroup: "primary", Endpoint: "chat-completions", APIBase: upstream.URL + "/primary"}
	backup := Candidate{DeploymentID: "backup", RoutingGroup: "backup", Endpoint: "chat-completions", APIBase: upstream.URL + "/backup"}
	server := NewServer(Config{Cooldown: time.Minute, Routes: []Route{{Alias: "demo", Candidates: []Candidate{primary, backup}}}})
	for range 2 {
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("fallback returned %d: %s", response.Code, response.Body)
		}
	}
	if primaryCalls.Load() != 1 {
		t.Fatalf("primary calls = %d, want one", primaryCalls.Load())
	}
}
