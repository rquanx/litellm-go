package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompatibilityUpstreamDispatch(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/v1/threads/abc%2Fdef?limit=3" ||
			r.Header.Get("Authorization") != "Bearer upstream-key" || string(body) != "request" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "data: fixture\n\n")
	}))
	defer upstream.Close()
	native := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	handler, err := withCompatibilityUpstream(native, upstream.URL, "/v1/models,/internal/v1/routes/*")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/threads/abc%2Fdef?limit=3", strings.NewReader("request"))
	request.Header.Set("Authorization", "Bearer upstream-key")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if result.Code != http.StatusAccepted || result.Body.String() != "data: fixture\n\n" ||
		result.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected proxy response: %d %s", result.Code, result.Body)
	}
	for _, route := range []string{"/v1/models", "/internal/v1/routes/sample"} {
		result = httptest.NewRecorder()
		handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, route, nil))
		if result.Code != http.StatusNotFound {
			t.Fatalf("unexpected native response for %s: %d", route, result.Code)
		}
	}
	if upstreamCalls != 1 {
		t.Fatalf("expected one upstream call, got %d", upstreamCalls)
	}
}
