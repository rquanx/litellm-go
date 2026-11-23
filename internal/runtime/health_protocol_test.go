package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestHealthProbesRealtimeBatchesAndVideo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("unexpected probe method or credential on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/realtime":
			if r.Method != http.MethodGet {
				t.Error("Realtime health probe requires GET")
			}
			if r.URL.Query().Get("model") != "gpt-realtime" {
				t.Errorf("unexpected Realtime model %s", r.URL.RawQuery)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			if _, _, err := conn.Read(r.Context()); err == nil {
				t.Error("Realtime probe sent an event")
			}
		case "/v1/batches":
			if r.Method != http.MethodGet {
				t.Error("Batch health probe requires GET")
			}
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("unexpected batch page size: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"data":[]}`)
		case "/v1/videos":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || string(body) != `{"model":"fixture-video","prompt":"test video generation"}` {
				t.Errorf("unexpected video request: %s %s", r.Method, body)
			}
			io.WriteString(w, `{"id":"video_fixture","status":"queued"}`)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{
		{Alias: "realtime", Candidates: []Candidate{{DeploymentID: "realtime", APIBase: upstream.URL, APIKey: "fixture-key",
			Provider: "openai", ModelID: "gpt-realtime", Endpoint: "chat-completions", HealthMode: "realtime"}}},
		{Alias: "batch", Candidates: []Candidate{{DeploymentID: "batch", APIBase: upstream.URL, APIKey: "fixture-key",
			Provider: "openai", ModelID: "fixture", Endpoint: "chat-completions", HealthMode: "batch"}}},
		{Alias: "video", Candidates: []Candidate{{DeploymentID: "video", APIBase: upstream.URL, APIKey: "fixture-key",
			Provider: "openai", ModelID: "fixture-video", Endpoint: "videos", HealthMode: "video_generation"}}},
	}})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"healthy_count":3`) {
		t.Fatalf("unexpected health response %d %s", response.Code, response.Body)
	}
}

func TestConnectionTestUsesScopedDeploymentID(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/target/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-upstream" {
			t.Errorf("connection test used an incorrect deployment: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer origin.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true,
		AuthorizeKey: func(key string) ([]string, bool) { return []string{"public"}, key == "limited" },
		Routes: []Route{
			{Alias: "public", Candidates: []Candidate{
				{DeploymentID: "first", APIBase: origin.URL, APIKey: "fixture-upstream", ModelID: "fixture", Provider: "openai", Endpoint: "chat-completions"},
				{DeploymentID: "target", APIBase: origin.URL + "/target", APIKey: "fixture-upstream", ModelID: "fixture", Provider: "openai", Endpoint: "chat-completions"},
			}},
			{Alias: "hidden-from-key", Candidates: []Candidate{
				{DeploymentID: "private", APIBase: origin.URL, APIKey: "fixture-upstream", ModelID: "fixture", Provider: "openai", Endpoint: "chat-completions"},
			}},
		},
	})
	for _, fixture := range []struct {
		path, method, token, body, contains string
		status                              int
	}{
		{"/test", "GET", "limited", "", `"route":"/test"`, http.StatusOK},
		{"/health/license", "GET", "limited", "", `"license_type":"community"`, http.StatusOK},
		{"/settings", "GET", "limited", "", `"num_callbacks":0`, http.StatusOK},
		{"/health/test_connection", "POST", "limited", `{"mode":"chat","model_info":{"id":"target"},"litellm_params":{"model":"public"}}`, `"status_code":429`, http.StatusOK},
		{"/health/test_connection", "POST", "limited", `{"model_info":{"id":"private"}}`, `"code":"model_not_found"`, http.StatusNotFound},
		{"/health/test_connection", "POST", "limited", `{"litellm_params":{"model":"public","api_base":"http://example.invalid"}}`, "", http.StatusBadRequest},
		{"/health/test_connection", "POST", "", `{"model_info":{"id":"target"}}`, "", http.StatusUnauthorized},
	} {
		request := httptest.NewRequest(fixture.method, fixture.path, strings.NewReader(fixture.body))
		request.Header.Set("Authorization", "Bearer "+fixture.token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != fixture.status || !strings.Contains(response.Body.String(), fixture.contains) ||
			strings.Contains(response.Body.String(), "fixture-upstream") {
			t.Fatalf("%s returned %d: %s", fixture.path, response.Code, response.Body)
		}
	}
}
