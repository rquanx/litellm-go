package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicBatchUsesDeploymentAffinity(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "fixture" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Error("missing provider headers")
		}
		switch r.URL.Path {
		case "/v1/messages/batches":
			var payload struct {
				Requests []struct{ Params map[string]any } `json:"requests"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload.Requests) != 1 ||
				payload.Requests[0].Params["model"] != "claude-fixture" {
				t.Error("batch did not map its model")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msgbatch_real","type":"message_batch"}`))
		case "/v1/messages/batches/msgbatch_real/results":
			w.Header().Set("Content-Type", "application/x-jsonl")
			w.Write([]byte("{\"custom_id\":\"item\"}\n"))
		default:
			t.Errorf("unexpected provider path %q", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	candidate := Candidate{Provider: "anthropic", DeploymentID: "provider", RoutingGroup: "demo",
		ModelID: "claude-fixture", APIBase: provider.URL, APIKey: "fixture"}
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/anthropic/v1/messages/batches",
		strings.NewReader(`{"requests":[{"params":{"model":"demo"}}]}`))
	request.Header.Set("x-api-key", "master")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	id := encodeResourceID("anthropic_batches", "provider", "msgbatch_real")
	if response.Code != 200 || !strings.Contains(response.Body.String(), id) {
		t.Fatalf("batch affinity not returned: %d %s", response.Code, response.Body)
	}
	request = httptest.NewRequest("GET", "/v1/messages/batches/"+id+"/results", nil)
	request.Header.Set("x-api-key", "master")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "{\"custom_id\":\"item\"}\n" {
		t.Fatalf("unexpected batch results: %d %s", response.Code, response.Body)
	}
}
