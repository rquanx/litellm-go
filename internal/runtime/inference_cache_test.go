package runtime

import (
	"bytes"
	"fmt"
	"mime/multipart"
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

func TestInferenceCacheStoresOnlyCompletedStreams(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\r\n\r\n")
		if requests.Add(1) == 1 {
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\r\n\r\ndata: [DONE]\r\n\r\n")
	}))
	defer upstream.Close()
	var event InferenceEvent
	server := NewServer(Config{
		Routes:         []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, ModelID: "native", Endpoint: "chat-completions"}}}},
		InferenceCache: InferenceCacheConfig{Enabled: true, TTL: time.Minute, Endpoints: map[string]bool{"chat-completions": true}},
		Callback:       func(value InferenceEvent) { event = value },
	})
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"demo","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !response.Flushed {
			t.Fatalf("stream was not flushed: %d %s", response.Code, response.Body)
		}
		return response
	}
	if first := call(); first.Header().Get("X-LiteLLM-Cache-Key") != "" || strings.Contains(first.Body.String(), "[DONE]") {
		t.Fatalf("unexpected incomplete stream: %v %s", first.Header(), first.Body)
	}
	second := call()
	if event.CacheHit || !strings.Contains(second.Body.String(), "[DONE]") {
		t.Fatalf("expected completed provider stream: %v %s", second.Header(), second.Body)
	}
	third := call()
	if requests.Load() != 2 || !event.CacheHit || event.Total != 3 || event.Spend == nil || *event.Spend != 0 ||
		third.Header().Get("X-LiteLLM-Cache-Key") == "" || third.Body.String() != strings.ReplaceAll(second.Body.String(), "\r\n", "\n") {
		t.Fatalf("expected cached stream with usage: %v %s, %+v", third.Header(), third.Body, event)
	}
}

func TestInferenceCacheUsesTranscriptionFileContent(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "transcription %d", requests.Add(1))
	}))
	defer upstream.Close()
	server := NewServer(Config{
		Routes:         []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, ModelID: "native", Endpoint: "transcription"}}}},
		InferenceCache: InferenceCacheConfig{Enabled: true, TTL: time.Minute, Endpoints: map[string]bool{"transcription": true}},
	})
	call := func(audio, expected string, hit bool) {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("model", "demo"); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("file", "fixture.wav")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(file, audio)
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
		request.Header.Set("Content-Type", form.FormDataContentType())
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != expected ||
			response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
			(response.Header().Get("X-LiteLLM-Cache-Key") != "") != hit {
			t.Fatalf("unexpected transcription response: %d %v %s", response.Code, response.Header(), response.Body)
		}
	}
	call("audio one", "transcription 1", false)
	call("audio one", "transcription 1", true)
	call("audio two", "transcription 2", false)
}
