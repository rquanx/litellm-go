package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
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
	ownInfo := send("GET", "/team/info?team_id=fixture", key, "", 200)
	if !strings.Contains(ownInfo, `"keys":[]`) {
		t.Fatalf("team-scoped key could see other credentials: %s", ownInfo)
	}
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
	send("POST", "/team/new", master, `{"team_id":"fixture","models":["first","second"]}`, 200)
	send("POST", "/team/member_add", master, `{"team_id":"fixture","member":{"user_id":"user","role":"user"},"allowed_models":["second"]}`, 200)
	bound := send("POST", "/key/generate", master, `{"team_id":"fixture","user_id":"user","models":["all-team-models"]}`, 200)
	if err := json.Unmarshal([]byte(bound), &user); err != nil || user.Key == "" {
		t.Fatal("could not issue a team member key")
	}
	send("POST", "/v1/chat/completions", user.Key, `{"model":"first","messages":[]}`, 403)
	send("POST", "/v1/chat/completions", user.Key, `{"model":"second","messages":[]}`, 200)
	send("POST", "/team/member_update", master, `{"team_id":"fixture","user_id":"user","role":"admin","allowed_models":["first"]}`, 200)
	send("POST", "/v1/chat/completions", user.Key, `{"model":"second","messages":[]}`, 403)
	send("POST", "/team/member_delete", master, `{"team_id":"fixture","user_id":"user"}`, 200)
	handler = newHandler()
	send("GET", "/v1/models", user.Key, "", 401)
	send("POST", "/organization/new", master, `{"organization_id":"new-org","organization_alias":"New Org","models":["second"]}`, 200)
	send("POST", "/organization/member_add", master, `{"organization_id":"new-org","member":{"user_id":"user","role":"internal_user"}}`, 200)
	bound = send("POST", "/key/generate", master, `{"organization_id":"new-org","user_id":"user"}`, 200)
	if err := json.Unmarshal([]byte(bound), &user); err != nil || user.Key == "" {
		t.Fatal("could not issue an organization member key")
	}
	send("GET", "/v1/models", user.Key, "", 200)
	send("PATCH", "/organization/member_update", master, `{"organization_id":"new-org","user_id":"user","role":"internal_user_viewer"}`, 200)
	send("GET", "/v1/models", user.Key, "", 401)
	send("DELETE", "/organization/member_delete", master, `{"organization_id":"new-org","user_id":"user"}`, 200)
	send("POST", "/organization/member_add", master, `{"organization_id":"new-org","member":{"user_id":"user","role":"internal_user"}}`, 200)
	send("GET", "/v1/models", user.Key, "", 401)
}

func TestTeamCallbackConfigurationPersists(t *testing.T) {
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Token: "fixture-master"})
	handler := registry.Handler(server)
	request := func(method, path, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w.Body.String()
	}
	request("POST", "/team/new", `{"team_id":"callback-team"}`, 200)
	request("POST", "/team/callback-team/callback", `{"callback_name":"generic_api","callback_type":"success","callback_vars":{"url":"https://example.invalid"}}`, 200)
	body := request("GET", "/team/callback-team/callback", "", 200)
	if !strings.Contains(body, `"success_callbacks":["generic_api"]`) {
		t.Fatalf("callback configuration was not returned: %s", body)
	}
	request("POST", "/team/callback-team/disable_logging", "", 200)
	body = request("GET", "/team/callback-team/callback", "", 200)
	if !strings.Contains(body, `"success_callbacks":[]`) {
		t.Fatalf("team logging callbacks were not disabled: %s", body)
	}
}

func TestTeamCallbackDispatchesInferenceEvent(t *testing.T) {
	received := make(chan struct{}, 1)
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
	w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Teams["callback-team"] = team{ID: "callback-team", Metadata: map[string]any{"logging": []any{
		map[string]any{"callback_name": "generic_api", "callback_type": "success", "callback_vars": map[string]any{"endpoint": callbackServer.URL}},
	}}}
	dispatch := registry.TeamCallback(context.Background())
	dispatch(runtime.InferenceEvent{Status: "success", SpendAttribution: &runtime.SpendAttribution{TeamID: "callback-team"}})
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("team callback did not receive the inference event")
	}
}

func TestTeamAdminManagesOnlyOwnMemberships(t *testing.T) {
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Users = map[string]internalUser{
		"admin": {ID:"admin", Role:"internal_user"},
		"member": {ID:"member", Role:"internal_user"},
	}
	registry.state.Teams["owned"] = team{ID:"owned", Members:[]teamMembership{{UserID:"admin", Role:"admin"}}}
	registry.state.Teams["other"] = team{ID:"other"}
	registry.state.Keys[keyDigest("team-admin")] = virtualKey{UserID:"admin"}
	registry.state.Keys[keyDigest("team-member")] = virtualKey{UserID:"member"}
	handler := registry.Handler(http.NotFoundHandler())
	for _, fixture := range []struct {
		operation, token, body string
		status int
	}{
		{"add", "team-member", `{"team_id":"owned","member":{"user_id":"member","role":"user"}}`, http.StatusForbidden},
		{"add", "team-admin", `{"team_id":"other","member":{"user_id":"member","role":"user"}}`, http.StatusForbidden},
		{"add", "team-admin", `{"team_id":"owned","member":{"user_id":"member","role":"user"}}`, http.StatusOK},
		{"delete", "team-admin", `{"team_id":"owned","user_id":"member"}`, http.StatusOK},
		{"add", "", `{"team_id":"owned","member":{"user_id":"member","role":"user"}}`, http.StatusUnauthorized},
	} {
		r := httptest.NewRequest(http.MethodPost, "/team/member_"+fixture.operation, strings.NewReader(fixture.body))
		r.Header.Set("Authorization", "Bearer "+fixture.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != fixture.status {
			t.Fatalf("%s returned %d: %s", fixture.operation, w.Code, w.Body)
		}
	}
}
