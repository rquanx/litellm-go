package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestVertexDiscoveryVectorSearch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/fixture/v1/projects/demo/locations/global/collections/registered/dataStores/store/servingConfigs/default_config:search" ||
			r.Header.Get("Authorization") != "Bearer vertex-fixture" || string(body) != `{"pageSize":10,"query":"first second"}` {
			t.Errorf("unexpected Discovery Engine request: %s %s %s", r.Method, r.URL.Path, body)
		}
		io.WriteString(w, `{"results":[{"id":"one","document":{"derivedStructData":{"title":"First","link":"https://example.com/doc","snippets":[{"snippet":"text"},{"htmlSnippet":"<b>more</b>"}]}}},{"id":"two","document":{"derivedStructData":{"title":"Second"}}},{"id":"three"}]}`)
	}))
	defer upstream.Close()

	candidate := Candidate{DeploymentID: "vertex-id", Provider: "vertex_ai", ModelID: "gemini-fixture", Endpoint: "gemini-generate",
		APIBase: upstream.URL + "/wrong-api", Vertex: &VertexConfig{
			Project: "demo", Location: "global", DiscoveryBase: upstream.URL + "/fixture",
			TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "vertex-fixture"}),
		}}
	server := NewServer(Config{
		Token: "master", AuthorizeInference: true,
		Routes:       []Route{{Alias: "vertex", Candidates: []Candidate{candidate}}},
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"vertex"}, token == "search-key" },
		VectorRegistryLookup: func(_ *http.Request, id string) (string, map[string]any, bool, bool, error) {
			return "vertex_ai/search_api", map[string]any{"model": "vertex", "vertex_collection_id": "registered"},
				id == "store" || id == "private", id == "store", nil
		},
	})
	for _, fixture := range []struct {
		path, body string
		status     int
	}{
		{"/v1/vector_stores/store/search", `{"query":["first","second"],"vertex_collection_id":"ignored"}`, http.StatusOK},
		{"/vector_stores/store/search", `{"query":"first second"}`, http.StatusOK},
		{"/v1/vector_stores/private/search", `{"query":"first second"}`, http.StatusNotFound},
		{"/v1/vector_stores/store/search", `{"query":"first second","model":"denied"}`, http.StatusBadRequest},
		{"/v1/vector_stores/unregistered/search", `{"query":"first second","custom_llm_provider":"vertex_ai/search_api","vertex_collection_id":"../bad"}`, http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodPost, fixture.path, strings.NewReader(fixture.body))
		request.Header.Set("Authorization", "Bearer search-key")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != fixture.status {
			t.Fatalf("%s: got %d, want %d: %s", fixture.path, response.Code, fixture.status, response.Body)
		}
		if fixture.status == http.StatusOK {
			want := `{"data":[{"attributes":{"document_id":"one","link":"https://example.com/doc","title":"First"},"content":[{"text":"text \u003cb\u003emore\u003c/b\u003e","type":"text"}],"file_id":"https://example.com/doc","filename":"First","score":1},{"attributes":{"document_id":"two","title":"Second"},"content":[{"text":"Second","type":"text"}],"file_id":"two","filename":"Second","score":0.5},{"attributes":{"document_id":"three"},"content":[{"text":"","type":"text"}],"file_id":"three","filename":"Unknown Document","score":0.3333333333333333}],"object":"vector_store.search_results.page","search_query":"first second"}`
			if strings.TrimSpace(response.Body.String()) != want {
				t.Fatalf("unexpected response %s", response.Body)
			}
		}
	}
}
