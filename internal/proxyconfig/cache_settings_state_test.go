package proxyconfig

import (
	"net/http/httptest"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestCacheSettingsPersistAndReplaceRuntimeCache(t *testing.T) {
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
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true})
	manager := &cacheSettingsManager{server: server, store: postgresModels(db, "master")}
	manager.store.name = "inference-cache"
	handler := manager.Handler(server, keys)
	call := func(method, path, token, body string, status int, fragment string) {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != status || !strings.Contains(result.Body.String(), fragment) {
			t.Fatalf("%s %s returned %d %s", method, path, result.Code, result.Body)
		}
	}
	call("GET", "/cache/ping", "master", "", 503, "Cache not initialized")
	call("POST", "/cache/settings", "invalid", `{"cache_settings":{"type":"local"}}`, 401, "master key")
	call("POST", "/cache/settings", "master", `{"cache_settings":{"type":"local","namespace":"fixture-private","ttl":75}}`, 200, "updated successfully")
	call("GET", "/cache/settings", "master", "", 200, `"namespace":"fixture-private"`)
	call("GET", "/cache/ping", "master", "", 200, `"cache_type":"local"`)
	var saved string
	if err := db.QueryRow("SELECT value::text FROM minato_proxy_state WHERE name='inference-cache'").Scan(&saved); err != nil || strings.Contains(saved, "fixture-private") {
		t.Fatalf("cache settings did not persist in encrypted form: %v", err)
	}
	restarted := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true})
	manager = &cacheSettingsManager{server: restarted, store: postgresModels(db, "master")}
	manager.store.name = "inference-cache"
	if err := manager.refresh(); err != nil {
		t.Fatal(err)
	}
	handler = manager.Handler(restarted, keys)
	call("GET", "/cache/ping", "master", "", 200, `"cache_type":"local"`)
	call("GET", "/cache/settings", "master", "", 200, `"ttl":75`)
	if redisURL := os.Getenv("MINATO_TEST_REDIS_URL"); redisURL != "" {
		body, err := json.Marshal(map[string]any{"cache_settings": map[string]any{"type": "redis", "url": redisURL}})
		if err != nil {
			t.Fatal(err)
		}
		call("POST", "/cache/settings/test", "master", string(body), 200, `"status":"success"`)
		call("POST", "/cache/settings", "master", string(body), 200, `"type":"redis"`)
		call("GET", "/cache/ping", "master", "", 200, `"cache_type":"redis"`)
		t.Cleanup(func() { manager.managed.Close() })
	}
}


func TestImportLiteLLMCacheSettings(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE minato_proxy_state (LIKE public.minato_proxy_state INCLUDING ALL);
		CREATE TEMP TABLE "LiteLLM_CacheConfig" (id text, cache_settings jsonb);
		INSERT INTO "LiteLLM_CacheConfig" VALUES ('cache_config', to_jsonb('{"type":"local","ttl":15}'::text));`); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []bool{true, false} {
		imported, err := ImportLiteLLMCache(db, "master")
		if err != nil || imported != expected {
			t.Fatalf("imported=%t, expected=%t: %v", imported, expected, err)
		}
	}
	store := postgresModels(db, "master")
	store.name = "inference-cache"
	var loaded cacheConnectionSettings
	if err := store.load(&loaded); err != nil || loaded.Type != "local" || loaded.TTL == nil || *loaded.TTL != 15 {
		t.Fatalf("could not reload imported settings: %v", err)
	}
}
