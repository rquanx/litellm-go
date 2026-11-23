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

func TestVertexMultimodalEmbeddingMapsMediaAndUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var fields struct {
			Instances []vertexEmbeddingInstance `json:"instances"`
			Parameters map[string]int `json:"parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/projects/example/locations/global/publishers/google/models/multimodalembedding@001:predict" ||
			r.Header.Get("Authorization") != "Bearer fixture" || fields.Parameters["dimension"] != 128 || len(fields.Instances) != 3 {
			t.Errorf("unexpected Vertex request %s: %+v", r.URL.Path, fields)
		} else if first, second, third := fields.Instances[0], fields.Instances[1], fields.Instances[2]; first.Text == nil ||
			*first.Text != "山 水" || first.Image["bytesBase64Encoded"] != "YWJj" || second.Video["gcsUri"] != "gs://fixture/movie.mp4" ||
			third.Image["gcsUri"] != "gs://fixture/photo.png" {
			t.Errorf("unexpected Vertex media instances: %+v", fields.Instances)
		}
		w.Write([]byte(`{"predictions":[{"textEmbedding":[1],"imageEmbedding":[2]},{"videoEmbeddings":[{"embedding":[3],"startOffsetSec":0,"endOffsetSec":2},{"embedding":[4],"startOffsetSec":2,"endOffsetSec":5}]},{"imageEmbedding":[5]}]}`))
	}))
	defer upstream.Close()
	candidate := Candidate{Endpoint: "google-embeddings", ModelID: "multimodalembedding@001", APIKey: "fixture", APIBase: upstream.URL,
		Vertex: &VertexConfig{Project: "example", Location: "global"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"demo","dimensions":128,"input":["山 水","data:image/png;base64,YWJj","gs://fixture/movie.mp4",{"image":{"gcsUri":"gs://fixture/photo.png"}}]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	expected := `{"data":[{"embedding":[1],"index":0,"object":"embedding"},{"embedding":[3],"index":1,"object":"embedding"},{"embedding":[4],"index":1,"object":"embedding"},{"embedding":[5],"index":2,"object":"embedding"}],"model":"demo","object":"list","usage":{"completion_tokens":0,"prompt_tokens":0,"prompt_tokens_details":{"character_count":2,"image_count":2,"video_length_seconds":5},"total_tokens":0}}`
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != expected {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
}

func TestVertexCustomEmbeddingEndpoints(t *testing.T) {
	for _, fixture := range []struct {
		model, inputField, predictions string
	}{
		{"bge/123", "prompt", `[[1],[2]]`},
		{"123", "inputs", `[[[1]],[[2]]]`},
	} {
		t.Run(fixture.model, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var fields struct {
					Instances []map[string]string `json:"instances"`
				}
				if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/projects/example/locations/global/endpoints/123:predict" ||
					len(fields.Instances) != 2 || fields.Instances[0][fixture.inputField] != "first" || fields.Instances[1][fixture.inputField] != "second" {
					t.Errorf("unexpected embedding request %s: %+v", r.URL.Path, fields)
				}
				w.Write([]byte(`{"predictions":` + fixture.predictions + `}`))
			}))
			defer upstream.Close()
			candidate := Candidate{Endpoint: "google-embeddings", ModelID: fixture.model, APIKey: "fixture", APIBase: upstream.URL,
				Vertex: &VertexConfig{Project: "example", Location: "global"}}
			server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
			request := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"demo","input":["first","second"]}`))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"embedding":[2],"index":1`) ||
				!strings.Contains(response.Body.String(), `"total_tokens":0`) {
				t.Fatalf("response = %d %s", response.Code, response.Body)
			}
		})
	}
}
