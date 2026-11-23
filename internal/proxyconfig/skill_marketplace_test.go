package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSkillMarketplacePublishesEnabledPlugins(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TEMP TABLE "LiteLLM_ClaudeCodePluginTable" (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, version TEXT, description TEXT, manifest_json TEXT,
		files_json TEXT, enabled BOOLEAN, created_at TIMESTAMP(3), updated_at TIMESTAMP(3), created_by TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := WithSkillMarketplace(http.NotFoundHandler(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, status int, want string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
	}
	base := "/claude-code/plugins"
	payload := `{"name":"fixture","source":{"source":"github","repo":"org/demo"},"version":"1.2.3"}`
	call("POST", base, payload, 200, `"action":"created"`)
	call("POST", base, payload, 200, `"action":"updated"`)
	call("GET", "/public/skill_hub", "", 200, `"count":1`)
	call("GET", "/claude-code/marketplace.json", "", 200, `"repo":"org/demo"`)
	call("POST", base+"/fixture/disable", "", 200, `"enabled":false`)
	call("GET", "/public/skill_hub", "", 200, `"count":0`)
	call("GET", base+"/fixture", "", 200, `"enabled":false`)
	call("DELETE", base+"/fixture", "", 200, `"status":"success"`)
	call("GET", base+"/fixture", "", 404, "Plugin not found")
}
