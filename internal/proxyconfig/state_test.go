package proxyconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/secretbox"
)

func TestPostgresProxyStateCompareAndSwap(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	name := "test-" + keyDigest(t.TempDir())
	defer db.Exec("DELETE FROM minato_proxy_state WHERE name = $1", name)
	first := &postgresState{db: db, name: name}
	second := &postgresState{db: db, name: name}
	if err := first.save(map[string]string{"state": "created"}); err != nil {
		t.Fatal(err)
	}
	var state map[string]string
	if err := second.load(&state); err != nil || state["state"] != "created" {
		t.Fatalf("saved state was not restored: %v", err)
	}
	if err := first.save(map[string]string{"state": "updated"}); err != nil {
		t.Fatal(err)
	}
	if err := second.save(map[string]string{"state": "stale"}); !errors.Is(err, errConcurrentState) {
		t.Fatalf("stale concurrent write returned %v", err)
	}
	if err := second.load(&state); err != nil || state["state"] != "updated" {
		t.Fatal("stale write changed committed state")
	}
}

func TestLiteLLMDeploymentSecretsAndVirtualKeys(t *testing.T) {
	key := sha256.Sum256([]byte("fixture-salt"))
	var nonce [24]byte
	nonce[0] = 7
	secret := base64.URLEncoding.EncodeToString(secretbox.Seal(nonce[:], []byte("synthetic-api-key"), &nonce, &key))
	encrypted, _ := json.Marshal(map[string]any{"api_key": secret, "model": "openai/fixture"})
	plain, err := decryptLiteLLMParams(encrypted, "fixture-salt")
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]string
	if json.Unmarshal(plain, &params) != nil || params["api_key"] != "synthetic-api-key" || params["model"] != "openai/fixture" {
		t.Fatal("LiteLLM SecretBox fields were not restored")
	}
	if _, err := decryptLiteLLMParams(encrypted, "wrong-salt"); err == nil {
		t.Fatal("encrypted deployment accepted an incorrect LiteLLM salt")
	}

	token := keyDigest("fixture-token")
	fixture := `{"token":"` + token + `","key_alias":"fixture","models":["demo"],"expires":"2030-01-01T00:00:01.123"}`
	digest, virtual, blocked, err := decodeLiteLLMKey([]byte(fixture))
	if err != nil || blocked || digest != token || virtual.Expires == nil ||
		len(virtual.Models) != 1 || virtual.Models[0] != "demo" {
		t.Fatalf("virtual key was not preserved: %v", err)
	}
	if _, _, _, err := decodeLiteLLMKey([]byte(`{"team_id":"fixture-team"}`)); err == nil {
		t.Fatal("import accepted an unsupported team scope")
	}
	if _, _, blocked, err := decodeLiteLLMKey([]byte(`{"team_id":"fixture-team","blocked":true}`)); err != nil || !blocked {
		t.Fatal("blocked LiteLLM key should be skipped")
	}
}

func TestPostgresImportsLiteLLMState(t *testing.T) {
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
	_, err = db.Exec(`CREATE TEMP TABLE minato_proxy_state (name TEXT PRIMARY KEY, revision BIGINT NOT NULL, value JSONB NOT NULL);
		CREATE TEMP TABLE "LiteLLM_ProxyModelTable" (model_id TEXT, model_name TEXT, litellm_params JSONB, model_info JSONB, created_at TIMESTAMP DEFAULT NOW());
		CREATE TEMP TABLE "LiteLLM_VerificationToken" (token TEXT, key_alias TEXT, models TEXT[], expires TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	const master = "fixture-master"
	t.Setenv("LITELLM_SALT_KEY", "fixture-source-salt")
	t.Setenv("MINATO_RUNTIME_STATE_KEY", "fixture-state-key")
	key := sha256.Sum256([]byte("fixture-source-salt"))
	var nonce [24]byte
	nonce[0] = 1
	encrypted := base64.URLEncoding.EncodeToString(secretbox.Seal(nonce[:], []byte("provider-secret"), &nonce, &key))
	params, _ := json.Marshal(map[string]string{"model": "openai/fixture", "api_base": "http://example.invalid", "api_key": encrypted})
	if _, err := db.Exec(`INSERT INTO "LiteLLM_ProxyModelTable" (model_id, model_name, litellm_params) VALUES ($1,$2,$3::jsonb)`,
		"upstream-deployment", "imported", string(params)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "LiteLLM_VerificationToken" VALUES ($1, 'fixture-key', ARRAY['imported'], '2030-01-01 00:00:00')`,
		keyDigest("fixture-token")); err != nil {
		t.Fatal(err)
	}
	if count, err := ImportLiteLLMModels(db, Config{MasterKey: master}); err != nil || count != 1 {
		t.Fatalf("model import count=%d, err=%v", count, err)
	}
	if count, err := ImportLiteLLMKeys(db); err != nil || count != 1 {
		t.Fatalf("key import count=%d, err=%v", count, err)
	}
	var state string
	if err := db.QueryRow(`SELECT value FROM minato_proxy_state WHERE name = 'models'`).Scan(&state); err != nil ||
		strings.Contains(state, "provider-secret") || strings.Contains(state, "upstream-deployment") {
		t.Fatal("imported deployment state was not encrypted")
	}
	var models modelState
	if err := postgresModels(db, master).load(&models); err != nil || len(models.Models) != 1 ||
		models.Models[0].Params.APIKey != "provider-secret" || models.Models[0].Info.ID != "upstream-deployment" {
		t.Fatalf("imported configuration could not be restored: %v", err)
	}
	registry, err := LoadPostgresKeys(master, db)
	if err != nil {
		t.Fatal(err)
	}
	if scope, allowed := registry.Authorize("fixture-token"); !allowed || len(scope) != 1 || scope[0] != "imported" {
		t.Fatal("imported key did not retain the original scope")
	}
}
