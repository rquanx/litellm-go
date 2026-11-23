package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMilvusSearchEmbedsQueryAndNormalizesDistance(t *testing.T) {
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			io.WriteString(w, `{"data":[{"index":0,"embedding":[0.5,0.25]}]}`)
		case "/vector/v2/vectordb/entities/search":
			var fields map[string]any
			err := json.NewDecoder(r.Body).Decode(&fields)
			payload, _ := json.Marshal(fields)
			if r.Header.Get("Authorization") != "Bearer milvus-fixture" || err != nil ||
				string(payload) != `{"annsField":"book_intro_vector","collectionName":"catalog","data":[[0.5,0.25]],"dbName":"docs","partitionNames":["p1"]}` {
				t.Errorf("unexpected Milvus request: %s", payload)
			}
			io.WriteString(w, `{"code":0,"data":[{"id":3,"distance":0.75,"snippet":"text","source":"mock"}]}`)
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer sink.Close()
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{
		{Alias: "milvus", Candidates: []Candidate{{DeploymentID: "vector-id", Provider: "milvus", Endpoint: "vector-stores", APIBase: sink.URL + "/vector", APIKey: "milvus-fixture", ModelID: "database"}}},
		{Alias: "embedding", Candidates: []Candidate{{DeploymentID: "embedding-id", Provider: "openai", Endpoint: "embeddings", APIBase: sink.URL, ModelID: "embed"}}},
	}, VectorRegistryLookup: func(_ *http.Request, id string) (string, map[string]any, bool, bool, error) {
		return "milvus", map[string]any{"model": "milvus", "litellm_embedding_model": "embedding", "milvus_text_field": "snippet", "milvus_db_name": "docs", "milvus_partition_names": []string{"p1"}}, true, id == "catalog", nil
	}})
	for _, id := range []string{"catalog", "denied"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/vector_stores/"+id+"/search", strings.NewReader(`{"query":["first","second"],"milvus_db_name":"ignored"}`))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, request)
		if id == "denied" {
			if w.Code != http.StatusNotFound {
				t.Fatalf("denied collection returned %d", w.Code)
			}
		} else if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"data":[{"attributes":{"source":"mock"},"content":[{"text":"text","type":"text"}],"file_id":null,"filename":null,"score":0.75}],"object":"vector_store.search_results.page","search_query":"first second"}` {
			t.Fatalf("unexpected search response %d %s", w.Code, w.Body)
		}
	}
}
