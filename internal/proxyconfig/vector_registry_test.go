package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestVectorRegistryPostgres(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := WithVectorRegistry(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, body string, status int, want string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != status || !strings.Contains(result.Body.String(), want) || strings.Contains(result.Body.String(), "fixture-secret") {
			t.Fatalf("%s %s returned %d %s", method, path, result.Code, result.Body.String())
		}
	}
	id := "registry-fixture-" + newEntityID()
	selectBody := `{"vector_store_id":"` + id + `"}`
	create := `{"vector_store_id":"` + id + `","custom_llm_provider":"s3_vectors","litellm_params":{"api_key":"fixture-secret","embedding_model":"embed"},"vector_store_metadata":{"origin":"fixture"}}`
	call("POST", "/vector_store/new", "invalid", create, 401, "token required")
	call("POST", "/vector_store/new", "master", create, 200, `"embedding_model":"embed"`)
	call("POST", "/vector_store/new", "master", create, 409, "already exists")
	call("POST", "/vector_store/info", "master", selectBody, 200, `"origin":"fixture"`)
	call("POST", "/vector_store/update", "master", `{"vector_store_id":"`+id+`","vector_store_name":"updated"}`, 200, `"vector_store_name":"updated"`)
	lookup := VectorRegistryLookup(db, keys)
	req := httptest.NewRequest(http.MethodPost, "/v1/vector_stores/"+id+"/search", nil)
	req.Header.Set("Authorization", "Bearer master")
	provider, params, found, allowed, err := lookup(req, id)
	if err != nil || !found || !allowed || provider != "s3_vectors" || params["embedding_model"] != "embed" || params["api_key"] != nil {
		t.Fatalf("unexpected registry routing result %s %v %t %t %v", provider, params, found, allowed, err)
	}
	call("GET", "/v1/vector_store/list?page_size=1", "master", "", 200, `"current_page":1`)
	call("POST", "/vector_store/delete", "master", selectBody, 200, "deleted successfully")
	call("POST", "/vector_store/info", "master", selectBody, 404, "not found")
}
