package proxyconfig

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
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

func TestMilvusIndexPassThroughUsesScopedRegisteredCollection(t *testing.T) {
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
	if _, err := WithVectorRegistry(http.NotFoundHandler(), db, keys); err != nil {
		t.Fatal(err)
	}
	name := "milvus-" + newEntityID()
	keys.state.Users = map[string]internalUser{"reader": {ID: "reader", Role: "internal_user"}}
	keys.state.Keys[keyDigest("limited")] = virtualKey{UserID: "reader", Models: []string{"vector"}, Metadata: map[string]any{
		"allowed_vector_store_indexes": []any{map[string]any{"index_name": name, "index_permissions": []any{"read"}}},
	}}
	if _, err := db.Exec(`INSERT INTO "LiteLLM_ManagedVectorStoresTable"
		(vector_store_id,custom_llm_provider,vector_store_name,litellm_params,user_id)
		VALUES ($1,'milvus',$1,'{"model":"vector","milvus_db_name":"docs"}','reader')`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "LiteLLM_ManagedVectorStoreIndexTable" (id,index_name,litellm_params)
		VALUES ($1,$1,jsonb_build_object('vector_store_name',$1::text,'vector_store_index','native_collection'))`, name); err != nil {
		t.Fatal(err)
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["collectionName"] != "native_collection" ||
			body["dbName"] != "docs" || r.Header.Get("Authorization") != "Bearer milvus" || !strings.HasSuffix(r.URL.Path, "/search") {
			t.Errorf("unexpected forwarded request: %v %s", body, r.URL.Path)
		}
		io.WriteString(w, `{"code":0}`)
	}))
	defer sink.Close()
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, AuthorizeKey: keys.Authorize,
		MilvusIndexLookup: MilvusIndexLookup(db, keys), Routes: []runtime.Route{{Alias: "vector", Candidates: []runtime.Candidate{{
			DeploymentID: "milvus", APIBase: sink.URL, Provider: "milvus", Endpoint: "vector-stores", APIKey: "milvus", ModelID: "collections",
		}}}},
	})
	for _, test := range []struct {
		op     string
		status int
	}{{"search", 200}, {"insert", 403}} {
		r := httptest.NewRequest(http.MethodPost, "/milvus/v2/vectordb/entities/"+test.op, strings.NewReader(`{"collectionName":"`+name+`","dbName":"denied"}`))
		r.Header.Set("Authorization", "Bearer limited")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s returned %d %s", test.op, w.Code, w.Body)
		}
	}
}
