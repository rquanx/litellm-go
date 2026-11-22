package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyHealthProbesAndDiagnostics(t *testing.T) {
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		DatabasePing: func(context.Context) error { return errors.New("fixture database failure") },
		CallbackNames: []string{"generic_api"},
	})
	request := func(method, path string, auth bool, want int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer fixture-master")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s %s: got %d, want %d", method, path, response.Code, want)
		}
		return response
	}
	for _, path := range []string{"/health/liveliness", "/health/liveness"} {
		if body := request(http.MethodGet, path, false, 200).Body.String(); !strings.Contains(body, "I'm alive!") {
			t.Fatalf("unexpected liveness body %q", body)
		}
		if got := request(http.MethodOptions, path, false, 200).Header().Get("Allow"); got != "GET, OPTIONS" {
			t.Fatalf("unexpected Allow header %q", got)
		}
	}
	body := request(http.MethodGet, "/health/readiness", false, 503).Body.String()
	if !strings.Contains(body, "disconnected") || strings.Contains(body, "generic_api") {
		t.Fatalf("unexpected public readiness body %q", body)
	}
	request(http.MethodGet, "/health/readiness/details", false, 401)
	if body := request(http.MethodGet, "/health/readiness/details", true, 503).Body.String(); !strings.Contains(body, "generic_api") {
		t.Fatalf("unexpected detailed readiness body %q", body)
	}
	if body := request(http.MethodGet, "/health/backlog", true, 200).Body.String(); !strings.Contains(body, `"in_flight_requests":1`) {
		t.Fatalf("unexpected backlog body %q", body)
	}
}

func TestModelHealthScopesProbesAndHidesUpstreamAddress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/allowed/v1/chat/completions" {
			t.Errorf("unexpected deployment probe: %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"allowed"}, token == "fixture-key" },
		Routes: []Route{
			{Alias: "allowed", Candidates: []Candidate{
				{DeploymentID: "allowed-id", ModelID: "fixture", RoutingGroup: "allowed", Endpoint: "chat-completions", APIBase: upstream.URL + "/allowed"},
				{DeploymentID: "blocked-id", ModelID: "fixture", RoutingGroup: "blocked", Endpoint: "chat-completions", APIBase: upstream.URL + "/blocked"},
			}},
			{Alias: "blocked", Candidates: []Candidate{
				{DeploymentID: "blocked-id", ModelID: "fixture", RoutingGroup: "blocked", Endpoint: "chat-completions", APIBase: upstream.URL + "/blocked"},
			}},
		},
	})
	for _, path := range []string{"/health", "/health?model_id=blocked-id"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer fixture-key")
		output := httptest.NewRecorder()
		server.ServeHTTP(output, req)
		if body := output.Body.String(); strings.Contains(body, "api_base") || strings.Contains(body, "blocked-id") {
			t.Fatalf("restricted deployment metadata was exposed: %s", body)
		}
		if path == "/health" && (output.Code != 200 || !strings.Contains(output.Body.String(), `"healthy_count":1`)) {
			t.Fatalf("unexpected health status %d: %s", output.Code, output.Body.String())
		}
		if path != "/health" && output.Code != 503 {
			t.Fatalf("unavailable target returned %d", output.Code)
		}
	}
}
