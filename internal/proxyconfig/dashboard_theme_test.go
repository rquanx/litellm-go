package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDashboardThemePersistsPublicImageURLs(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TEMP TABLE minato_proxy_state (LIKE public.minato_proxy_state INCLUDING ALL)"); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("default asset"))
	})
	if _, err := db.Exec(`CREATE TEMP TABLE "LiteLLM_AuditLog" (
		id TEXT PRIMARY KEY, updated_at TIMESTAMP NOT NULL,
		changed_by TEXT NOT NULL, changed_by_api_key TEXT NOT NULL, action TEXT NOT NULL,
		table_name TEXT NOT NULL, object_id TEXT NOT NULL, before_value JSONB, updated_values JSONB
	)`); err != nil {
		t.Fatal(err)
	}
	config := Config{StoreAuditLogs: true}
	audit := WithManagementAudit(base, db, keys)
	handler := config.WithDashboardTheme(audit, keys, db)
	call := func(method, path, token, body string, code int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != code {
			t.Fatalf("%s %s returned %d: %s", method, path, result.Code, result.Body)
		}
		return result
	}
	call("PATCH", "/update/ui_theme_settings", "master", `{"logo_url":"file:///logo.jpg"}`, 400)
	call("PATCH", "/update/ui_theme_settings", "bad", `{}`, 401)
	call("PATCH", "/update/ui_theme_settings", "master", `{"logo_url":"https://example.invalid/fixture.png"}`, 200)
	handler = config.WithDashboardTheme(audit, keys, db)
	theme := call("GET", "/get/ui_theme_settings", "", "", 200)
	if !strings.Contains(theme.Body.String(), `"logo_url":"https://example.invalid/fixture.png"`) {
		t.Fatalf("theme did not persist: %s", theme.Body)
	}
	image := call("GET", "/get_image", "", "", 307)
	if image.Header().Get("Location") != "https://example.invalid/fixture.png" {
		t.Fatalf("unexpected image redirect: %s", image.Header().Get("Location"))
	}
	call("PATCH", "/update/ui_theme_settings", "master", `{}`, 200)
	fallback := call("GET", "/get_image", "", "", 200)
	if fallback.Body.String() != "default asset" {
		t.Fatalf("expected default branding: %s", fallback.Body)
	}
	call("GET", "/audit", "bad", "", 401)
	logged := call("GET", "/audit?table_name=LiteLLM_Config&page_size=1", "master", "", 200)
	if !strings.Contains(logged.Body.String(), `"total":2`) ||
		!strings.Contains(logged.Body.String(), `"logo_url_configured":false`) ||
		strings.Contains(logged.Body.String(), "example.invalid") {
		t.Fatalf("audit did not capture redacted theme changes: %s", logged.Body)
	}
	dbKeys, err := LoadPostgresKeys("master", db)
	if err != nil {
		t.Fatal(err)
	}
	dbKeys.SetStoreAuditLogs(true)
	handler = WithManagementAudit(dbKeys.Handler(base), db, dbKeys)
	generated := call("POST", "/key/generate", "master", `{"key_alias":"audit-fixture"}`, 200)
	var key struct {
		Token string `json:"key"`
		Digest string `json:"token_id"`
	}
	if err := json.Unmarshal(generated.Body.Bytes(), &key); err != nil || key.Token == "" {
		t.Fatalf("could not decode fixture key: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"keys":[]string{key.Token}})
	call("POST", "/key/delete", "master", string(body), 200)
	logged = call("GET", "/audit?object_key_hash="+key.Digest, "master", "", 200)
	if !strings.Contains(logged.Body.String(), `"total":2`) ||
		!strings.Contains(logged.Body.String(), `"action":"deleted"`) || strings.Contains(logged.Body.String(), key.Token) {
		t.Fatalf("key audit entries were missing or exposed the token: %s", logged.Body)
	}
}

