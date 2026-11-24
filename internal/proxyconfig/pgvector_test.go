package proxyconfig

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestPGVectorCreateAndSearchWithDeploymentAffinity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer connector-key" {
			t.Errorf("unexpected connector request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/connector/v1/vector_stores":
			if string(body) != `{"chunking_strategy":null,"expires_after":null,"file_ids":null,"metadata":null,"name":"fixture"}` {
				t.Errorf("unexpected create request: %s", body)
			}
			io.WriteString(w, `{"id":"native","object":"vector_store"}`)
		case "/connector/v1/vector_stores/native/search":
			if string(body) != `{"filters":null,"max_num_results":2,"query":["first","second"],"ranking_options":null,"rewrite_query":false}` {
				t.Errorf("unexpected search request: %s", body)
			}
			io.WriteString(w, `{"object":"vector_store.search_results.page","data":[{"file_id":"connector-file","score":0.5}]}`)
		default:
			t.Errorf("unexpected connector path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	t.Setenv("PG_VECTOR_API_BASE", upstream.URL+"/connector")
	t.Setenv("PG_VECTOR_API_KEY", "connector-key")
	cfg, err := Load(strings.NewReader(`
model_list:
  - model_name: connector
    litellm_params:
      model: pg_vector/documents
    model_info:
      id: connector-id
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{
		Token: "master", Routes: cfg.Routes, AuthorizeInference: true,
		AuthorizeKey: func(key string) ([]string, bool) {
			if key == "limited" {
				return []string{"connector"}, true
			}
			return []string{"other"}, key == "denied"
		},
	})
	post := func(token, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	created := post("limited", "/v1/vector_stores", `{"name":"fixture","model":"connector","custom_llm_provider":"pg_vector"}`)
	var result struct{ ID string }
	if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &result) != nil || !strings.HasPrefix(result.ID, "minato_") {
		t.Fatalf("create response: %d %s", created.Code, created.Body)
	}
	path := "/vector_stores/" + result.ID + "/search"
	query := `{"query":["first","second"],"max_num_results":2,"rewrite_query":false}`
	found := post("limited", path, query)
	if found.Code != http.StatusOK || !strings.Contains(found.Body.String(), `"file_id":"connector-file"`) {
		t.Fatalf("search response: %d %s", found.Code, found.Body)
	}
	denied := post("denied", path, query)
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("unexpected cross-scope response: %d %s", denied.Code, denied.Body)
	}
}
