package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBedrockTitanEmbeddingsAggregatesInputOrderAndUsage(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Text string `json:"inputText"`
			Dimensions int `json:"dimensions"`
			Types []string `json:"embeddingTypes"`
		}
		index := calls.Add(1)
		if r.URL.Path != "/model/amazon.titan-embed-text-v2:0/invoke" || r.Header.Get("Authorization") != "Bearer fixture" ||
			json.NewDecoder(r.Body).Decode(&input) != nil || input.Text != fmt.Sprintf("item%d", index) ||
			input.Dimensions != 256 || len(input.Types) != 1 || input.Types[0] != "binary" {
			t.Errorf("unexpected Titan call: %s, %+v", r.URL, input)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"embeddingsByType":{"binary":[%d]},"inputTextTokenCount":2}`, index)
	}))
	defer provider.Close()
	candidate := Candidate{APIBase: provider.URL, ModelID: "amazon.titan-embed-text-v2:0", Endpoint: "bedrock-embeddings",
		Provider: "bedrock", APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}}
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{candidate}}}})
	request := httptest.NewRequest("POST", "/v1/embeddings",
		strings.NewReader(`{"model":"demo","input":["item1","item2"],"dimensions":256,"encoding_format":"base64"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || calls.Load() != 2 || !strings.Contains(response.Body.String(), `"total_tokens":4`) ||
		!strings.Contains(response.Body.String(), `"embedding":[2],"index":1`) {
		t.Fatalf("unexpected embeddings response: %d %s", response.Code, response.Body)
	}
}
