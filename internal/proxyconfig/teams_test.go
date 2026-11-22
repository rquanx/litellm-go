package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestTeamRestrictionsOnExistingKeys(t *testing.T) {
	const master = "master"
	state := filepath.Join(t.TempDir(), "keys.json")
	legacy := `{"` + keyDigest("legacy") + `":{"models":[]}}`
	if err := os.WriteFile(state, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primary.Close()
	var backupCalls atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"backup"}}]}`))
	}))
	defer backup.Close()

	newHandler := func() http.Handler {
		registry, err := LoadKeys(master, state)
		if err != nil {
			t.Fatal(err)
		}
		first := runtime.Candidate{Endpoint: "chat-completions", ModelID: "first", RoutingGroup: "first", APIBase: primary.URL}
		second := runtime.Candidate{Endpoint: "chat-completions", ModelID: "second", RoutingGroup: "second", APIBase: backup.URL}
		return registry.Handler(runtime.NewServer(runtime.Config{
			AuthorizeInference: true, AuthorizeKey: registry.Authorize, Token: master,
			Routes: []runtime.Route{
				{Alias: "first", Candidates: []runtime.Candidate{first, second}},
				{Alias: "second", Candidates: []runtime.Candidate{second}},
			},
		}))
	}
	handler := newHandler()
	send := func(method, path, token, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, response.Code, status, response.Body.String())
		}
		return response.Body.String()
	}
	legacyModels := send("GET", "/v1/models", "legacy", "", 200)
	if !strings.Contains(legacyModels, "first") || !strings.Contains(legacyModels, "second") {
		t.Fatal("legacy unscoped key lost model access")
	}
	send("POST", "/team/new", master, `{"team_id":"fixture","models":["first"]}`, 200)
	send("POST", "/team/new", master, `{"team_id":"fixture","models":["second"]}`, 400)
	generate := func(models string) string {
		t.Helper()
		response := send("POST", "/key/generate", master, `{"team_id":"fixture","models":`+models+`}`, 200)
		var key struct{ Key string }
		if err := json.Unmarshal([]byte(response), &key); err != nil || key.Key == "" {
			t.Fatal("missing issued key")
		}
		return key.Key
	}
	key := generate(`["all-team-models"]`)
	disjoint := generate(`["second"]`)
	if ownModels := send("GET", "/v1/models", key, "", 200); !strings.Contains(ownModels, "first") || strings.Contains(ownModels, "second") {
		t.Fatalf("incorrect team model list: %s", ownModels)
	}
	if noModels := send("GET", "/v1/models", disjoint, "", 200); strings.Contains(noModels, "first") || strings.Contains(noModels, "second") {
		t.Fatalf("disjoint model scopes exposed models: %s", noModels)
	}
	send("GET", "/team/info?team_id=fixture", key, "", 401)
	info := send("GET", "/team/info?team_id=fixture", master, "", 200)
	if strings.Contains(info, key) || strings.Contains(info, keyDigest(key)) {
		t.Fatal("team info exposed credential material")
	}
	send("POST", "/v1/chat/completions", key, `{"model":"first","messages":[]}`, 503)
	if backupCalls.Load() != 0 {
		t.Fatal("fallback exceeded the team model scope")
	}
	send("POST", "/team/update", master, `{"team_id":"fixture","models":["first","second"]}`, 200)
	send("POST", "/v1/chat/completions", key, `{"model":"first","messages":[]}`, 200)
	send("POST", "/v1/chat/completions", disjoint, `{"model":"first","messages":[]}`, 403)
	send("POST", "/team/block", master, `{"team_id":"fixture"}`, 200)
	handler = newHandler()
	send("GET", "/v1/models", key, "", 401)
	send("POST", "/team/unblock", master, `{"team_id":"fixture"}`, 200)
	send("GET", "/v1/models", key, "", 200)
	send("POST", "/team/delete", master, `{"team_ids":["fixture"]}`, 200)
	send("POST", "/team/new", master, `{"team_id":"fixture"}`, 200)
	handler = newHandler()
	send("GET", "/v1/models", key, "", 401)
	send("GET", "/v1/models", "legacy", "", 200)

	send("POST", "/organization/new", master, `{"organization_id":"org","organization_alias":"Fixture","models":["second"]}`, 200)
	send("POST", "/team/update", master, `{"team_id":"fixture","organization_id":"org","models":["first"]}`, 200)
	orgKey := generate(`["all-team-models"]`)
	if noModels := send("GET", "/v1/models", orgKey, "", 200); strings.Contains(noModels, "first") || strings.Contains(noModels, "second") {
		t.Fatalf("disjoint organization and team scopes exposed models: %s", noModels)
	}
	send("PATCH", "/organization/update", master, `{"organization_id":"org","models":["first","second"]}`, 200)
	send("POST", "/v1/chat/completions", orgKey, `{"model":"first","messages":[]}`, 503)
	send("POST", "/v1/chat/completions", orgKey, `{"model":"second","messages":[]}`, 403)
	organizations := send("GET", "/organization/list?org_alias=fixture", master, "", 200)
	if !strings.Contains(organizations, `"team_id":"fixture"`) {
		t.Fatal("organization list omitted its team")
	}
	send("DELETE", "/organization/delete", master, `{"organization_ids":["org","unknown"]}`, 404)
	send("GET", "/team/info?team_id=fixture", master, "", 200)
	send("DELETE", "/organization/delete", master, `{"organization_ids":["org"]}`, 200)
	handler = newHandler()
	send("GET", "/team/info?team_id=fixture", master, "", 404)
	send("GET", "/v1/models", orgKey, "", 401)
	send("GET", "/v1/models", "legacy", "", 200)

	response := send("POST", "/user/new", master, `{"user_id":"user","user_email":"test@example.invalid","models":["second"]}`, 200)
	var user struct{ Key string }
	if err := json.Unmarshal([]byte(response), &user); err != nil || user.Key == "" {
		t.Fatal("user creation did not return a credential")
	}
	send("POST", "/v1/chat/completions", user.Key, `{"model":"first","messages":[]}`, 403)
	send("POST", "/v1/chat/completions", user.Key, `{"model":"second","messages":[]}`, 200)
	send("POST", "/user/update", master, `{"user_email":"test@example.invalid","user_role":"internal_user_viewer"}`, 200)
	handler = newHandler()
	send("GET", "/v1/models", user.Key, "", 401)
	send("POST", "/user/update", master, `{"user_id":"user","user_role":"internal_user","models":["first"]}`, 200)
	send("POST", "/v1/chat/completions", user.Key, `{"model":"second","messages":[]}`, 403)
	send("POST", "/user/delete", master, `{"user_ids":["user"]}`, 200)
	send("POST", "/user/new", master, `{"user_id":"user","auto_create_key":false}`, 200)
	send("GET", "/v1/models", user.Key, "", 401)
}
