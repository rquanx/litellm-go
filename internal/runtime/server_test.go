package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesRouteAndFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		if body["model"] != "backup-model" {
			t.Fatalf("unexpected model %v", body["model"])
		}
		if request.Header.Get("x-opencode-session") != "session-1" {
			t.Fatalf("session header was not forwarded")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chat-1","object":"chat.completion","created":1,"model":"backup-model","choices":[{"message":{"role":"assistant","content":"ready"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`))
	}))
	defer backup.Close()

	server := NewServer(Config{Token: "runtime-token", HTTPClient: backup.Client()})
	putRoute(t, server, Route{Alias: "demo.production.classify", Version: "1", Candidates: []Candidate{
		{SourceID: "primary", ModelID: "primary-model", APIBase: primary.URL, APIKey: "one", Endpoint: "responses"},
		{SourceID: "opencode", ModelID: "backup-model", APIBase: backup.URL, APIKey: "two", Endpoint: "chat-completions"},
	}})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo.production.classify","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer runtime-token")
	request.Header.Set("x-opencode-session", "session-1")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("expected fallback response, got %d: %s", result.Code, result.Body.String())
	}
	if result.Header().Get("X-Minato-Runtime-Source") != "opencode" || result.Header().Get("X-Minato-Runtime-Attempts") != "2" {
		t.Fatalf("unexpected runtime metadata: source=%q attempts=%q", result.Header().Get("X-Minato-Runtime-Source"), result.Header().Get("X-Minato-Runtime-Attempts"))
	}
	var response map[string]any
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["object"] != "response" || response["model"] != "demo.production.classify" {
		t.Fatalf("unexpected normalized response %v", response)
	}
}

func TestClientErrorDoesNotFallback(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{HTTPClient: upstream.Client()})
	putRoute(t, server, Route{Alias: "demo", Version: "1", Candidates: []Candidate{
		{ModelID: "model-a", APIBase: upstream.URL, Endpoint: "responses"},
		{ModelID: "model-b", APIBase: upstream.URL, Endpoint: "responses"},
	}})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"hello"}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("expected one client-error attempt, status=%d calls=%d", result.Code, calls)
	}
}

func TestRouteAuthorizationAndModels(t *testing.T) {
	server := NewServer(Config{Token: "token"})
	request := httptest.NewRequest(http.MethodPut, "/internal/v1/routes/demo", strings.NewReader(`{"alias":"demo","version":"1","candidates":[{"modelId":"a","apiBase":"http://localhost","endpoint":"responses"}]}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusUnauthorized { t.Fatalf("expected 401, got %d", result.Code) }
	request.Header.Set("Authorization", "Bearer token")
	result = httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK { t.Fatalf("expected route publication, got %d", result.Code) }
	models := httptest.NewRecorder()
	server.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(models.Body.String(), "demo") { t.Fatalf("route is missing from models: %s", models.Body.String()) }
	_ = io.Discard
}

func TestSSEForwardsChunksAndFlushes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("upstream does not support flushing")
		}
		_, _ = writer.Write([]byte("data: first\n\n"))
		flusher.Flush()
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	server := NewServer(Config{HTTPClient: upstream.Client()})
	putRoute(t, server, Route{Alias: "demo", Version: "1", Candidates: []Candidate{{ModelID: "model-a", APIBase: upstream.URL, Endpoint: "chat-completions"}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Accept", "text/event-stream")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", result.Code, result.Body.String())
	}
	if result.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected SSE content type, got %q", result.Header().Get("Content-Type"))
	}
	if result.Body.String() != "data: first\n\ndata: [DONE]\n\n" {
		t.Fatalf("unexpected SSE body %q", result.Body.String())
	}
}

func TestSSERejectsCrossProtocolCandidate(t *testing.T) {
	server := NewServer(Config{})
	putRoute(t, server, Route{Alias: "demo", Version: "1", Candidates: []Candidate{{ModelID: "model-a", APIBase: "http://127.0.0.1:1", Endpoint: "chat-completions"}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","stream":true,"input":"hello"}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", result.Code, result.Body.String())
	}
}

func putRoute(t *testing.T, server *Server, route Route) {
	t.Helper()
	body, _ := json.Marshal(route)
	request := httptest.NewRequest(http.MethodPut, "/internal/v1/routes/"+route.Alias, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer runtime-token")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK { t.Fatalf("route publication failed: %d %s", result.Code, result.Body.String()) }
}
