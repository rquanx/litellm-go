package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoogleEmbeddingsMapsDeveloperAndVertexProtocols(t *testing.T) {
	for _, fixture := range []struct {
		name string
		path string
		vector string
		field string
		vertex *VertexConfig
	}{
		{"developer", "/v1beta/models/gemini-embedding-001:batchEmbedContents", `{"embeddings":[{"values":[1]}]}`, "requests", nil},
		{"vertex", "/v1/projects/example/locations/global/publishers/google/models/gemini-embedding-001:predict",
			`{"predictions":[{"embeddings":{"values":[1],"statistics":{"token_count":3}}}]}`, "instances",
			&VertexConfig{Project: "example", Location: "global"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]json.RawMessage
				if r.URL.Path != fixture.path || json.NewDecoder(r.Body).Decode(&payload) != nil || payload[fixture.field] == nil {
					t.Errorf("unexpected embedding request: %s %v", r.URL.Path, payload)
				}
				if fixture.vertex != nil && r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("Vertex request lost its access token")
				}
				if fixture.vertex == nil && r.Header.Get("x-goog-api-key") != "fixture" {
					t.Error("Developer API request lost its key")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(fixture.vector))
			}))
			defer upstream.Close()
			candidate := Candidate{Endpoint: "google-embeddings", ModelID: "gemini-embedding-001",
				APIKey: "fixture", APIBase: upstream.URL, Vertex: fixture.vertex}
			server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
			request := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"demo","input":["fixture"],"dimensions":128}`))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"embedding":[1]`) ||
				(fixture.vertex != nil && !strings.Contains(response.Body.String(), `"total_tokens":3`)) {
				t.Fatalf("response = %d %s", response.Code, response.Body)
			}
		})
	}
}
