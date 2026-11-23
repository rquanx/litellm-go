package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestHealthProbesRealtimeAndListsBatches(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("unexpected probe method or credential on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/v1/realtime":
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
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("unexpected batch page size: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"data":[]}`)
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
	}})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"healthy_count":2`) {
		t.Fatalf("unexpected health response %d %s", response.Code, response.Body)
	}
}
