package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisioningSettingsApplyToNewUsersAndTeams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.json")
	keys, err := LoadKeys("master", path)
	if err != nil {
		t.Fatal(err)
	}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(method, path, body string, status int, want string) {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer master")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != status || !strings.Contains(result.Body.String(), want) {
			t.Fatalf("%s %s returned %d %s", method, path, result.Code, result.Body)
		}
	}
	call("PATCH", "/update/default_team_settings", `{"models":["approved"],"rpm_limit":12,"team_member_permissions":["/key/list"]}`, 200, "success")
	call("POST", "/team/new", `{"team_id":"default-team"}`, 200, `"rpm_limit":12`)
	call("PATCH", "/update/internal_user_settings", `{"user_role":"internal_user","models":["approved"],"teams":["default-team"]}`, 200, "success")
	reloaded, err := LoadKeys("master", path)
	if err != nil {
		t.Fatal(err)
	}
	handler = reloaded.Handler(http.NotFoundHandler())
	call("POST", "/user/new", `{"user_id":"with-defaults","auto_create_key":false}`, 200, `"models":["approved"]`)
	call("GET", "/team/info?team_id=default-team", "", 200, "with-defaults")
	if settings := reloaded.ssoUserDefaultsLocked(); settings == nil || len(settings.Teams) != 1 {
		t.Fatal("provisioning defaults did not persist")
	}
	sso := &uiSSOServer{keys: reloaded, config: uiSSOConfig{provider: "generic"}}
	if _, _, ok := sso.issueSession(httptest.NewRecorder(), ssoIdentity{
		Subject: "directory-subject", Email: "directory`example.invalid", Verified: true,
	}); !ok {
		t.Fatal("verified SSO login did not use stored defaults")
	}
	if members := reloaded.state.Teams["default-team"].Members; len(members) != 2 {
		t.Fatalf("default team did not get both new users: %v", members)
	}
}

