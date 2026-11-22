package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiVectorStoreCreateAndGroundedSearch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("x-goog-api-key") != "fixture" {
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("invalid provider request: %v", err)
		}
		switch r.URL.Path {
		case "/v1beta/fileSearchStores":
			if string(request["displayName"]) != `"Test store"` {
				t.Errorf("incorrect Gemini store name %s", request["displayName"])
			}
			fmt.Fprint(w, `{"name":"fileSearchStores/abc","displayName":"Test store","createTime":"2026-01-02T00:00:00Z"}`)
		case "/v1beta/models/gemini-demo:generateContent":
			if !strings.Contains(string(request["tools"]), `"file_search_store_names":["fileSearchStores/abc"]`) ||
				!strings.Contains(string(request["tools"]), `category = \"books\"`) ||
				!strings.Contains(string(request["contents"]), `"find it"`) {
				t.Errorf("incorrect Gemini search request: %s %s", request["tools"], request["contents"])
			}
			fmt.Fprint(w, `{"candidates":[{"groundingMetadata":{"groundingChunks":[{"retrievedContext":{"text":"record","uri":"files/doc","title":"Doc"}}],"groundingSupports":[{"segment":{"text":"record"}},{"segment":{"text":"citation"},"groundingChunkIndices":[0],"confidenceScores":[0.7]}]}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		Routes: []Route{{Alias: "demo", Candidates: []Candidate{{APIBase: upstream.URL, APIKey: "fixture", ModelID: "gemini-demo",
			Provider: "gemini", Endpoint: "gemini-generate", DeploymentID: "gemini-deployment"}}}},
	})
	post := func(path, input string) map[string]any {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(input))
		request.Header.Set("Authorization", "Bearer master")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var data map[string]any
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &data) != nil {
			t.Fatalf("%s returned %d: %s", path, response.Code, response.Body)
		}
		return data
	}
	store := post("/vector_stores", `{"model":"demo","custom_llm_provider":"gemini","name":"Test store"}`)
	id := encodeResourceID("vector_stores", "gemini-deployment", "abc")
	if store["id"] != id || store["created_at"] != float64(1767312000) || store["status"] != "completed" {
		t.Fatalf("incorrect vector store response: %v", store)
	}
	result := post("/v1/vector_stores/"+id+"/search", `{"query":["find","it"],"filters":{"category":"books"}}`)
	items, _ := result["data"].([]any)
	if len(items) != 2 || result["search_query"] != "find it" ||
		items[0].(map[string]any)["file_id"] != "files/doc" || items[1].(map[string]any)["score"] != 0.7 {
		t.Fatalf("incorrect Gemini grounding results: %v", result)
	}
}
