package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMemoryUsesExistingSchemaAndScope(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TEMP TABLE "LiteLLM_MemoryTable" (
		memory_id TEXT PRIMARY KEY,key TEXT NOT NULL UNIQUE,value TEXT NOT NULL,metadata JSONB,user_id TEXT,team_id TEXT,
		created_at TIMESTAMP DEFAULT now(),created_by TEXT,updated_at TIMESTAMP DEFAULT now(),updated_by TEXT);
		INSERT INTO "LiteLLM_MemoryTable" (memory_id,key,value,user_id,team_id)
		VALUES ('existing-id','namespace/existing','original','owner','team-demo')`)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = map[string]internalUser{"owner": {Role: "internal_user"}, "peer": {Role: "internal_user"}}
	keys.state.Teams = map[string]team{"team-demo": {}}
	keys.state.Keys[keyDigest("owner-key")] = virtualKey{UserID: "owner", TeamID: "team-demo"}
	keys.state.Keys[keyDigest("peer-key")] = virtualKey{UserID: "peer", TeamID: "team-demo"}
	handler, err := WithMemoryManagement(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, body string, status int, contains string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), contains) {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
	}
	path := "/v1/memory/namespace/existing"
	call("GET", path, "peer-key", "", 200, "original")
	call("PUT", path, "peer-key", `{"value":"wrong"}`, 403, "denied")
	call("PUT", path, "owner-key", `{"value":"updated","metadata":"tag"}`, 200, `"metadata":"tag"`)
	call("GET", "/v1/memory?key_prefix=namespace/", "master", "", 200, `"total":1`)
	call("POST", "/v1/memory", "master", `{"key":"namespace/existing","value":"duplicate"}`, 409, "already exists")
	call("DELETE", path, "owner-key", "", 200, `"deleted":true`)
	call("GET", path, "master", "", 404, "not found")
}
