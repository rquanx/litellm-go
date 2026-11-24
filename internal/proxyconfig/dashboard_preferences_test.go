package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestDashboardPreferencesApplyAccessAndReadiness(t *testing.T) {
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
	keys.state.Keys[keyDigest("member")] = virtualKey{UserID: "member"}
	keys.state.Users = map[string]internalUser{"member": {ID:"member", Role:"internal_user"}, "unrelated": {ID:"unrelated", Email:"private@example.invalid"}}
	keys.state.Organizations = map[string]organization{"org": {ID: "org", Members: []organizationMember{{UserID:"member"}}}}
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true})
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := Config{}.WithDashboardPreferences(keys.Handler(base), keys, db, server, true)
	call := func(method, path, token, body string, want int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != want {
			t.Fatalf("%s %s returned %d: %s", method, path, result.Code, result.Body)
		}
		return result.Body.String()
	}
	call("GET", "/public/model_hub", "", "", 200)
	call("PATCH", "/update/ui_settings", "master",
		`{"require_auth_for_public_ai_hub":true,"allow_public_health_readiness_details":true,"disable_team_admin_delete_team_user":true}`, 200)
	call("PATCH", "/update/ui_settings", "master", `{"enabled_ui_pages_internal_users":["models"]}`, 200)
	call("GET", "/public/model_hub", "", "", 401)
	call("GET", "/public/model_hub", "member", "", 200)
	call("POST", "/team/member_delete", "member", "{}", 403)
	call("POST", "/team/member_delete", "master", "{}", 400)
	if details := call("GET", "/health/readiness", "", "", 200); !strings.Contains(details, `"litellm_version"`) {
		t.Fatalf("readiness details missing: %s", details)
	}
	if result := call("GET", "/user/filter/ui", "member", "", 200); !strings.Contains(result, "unrelated") {
		t.Fatalf("default directory search missing user: %s", result)
	}
	call("PATCH", "/update/ui_settings", "master", `{"scope_user_search_to_org":true}`, 200)
	if result := call("GET", "/user/filter/ui", "member", "", 200); !strings.Contains(result, "member") || strings.Contains(result, "unrelated") {
		t.Fatalf("directory search did not respect organization: %s", result)
	}
	call("POST", "/key/generate", "master", `{"key":"sk-fixture-custom"}`, 200)
	call("POST", "/key/generate", "master", `{"key":"sk-fixture-custom"}`, 409)
	call("PATCH", "/update/ui_settings", "master", `{"disable_custom_api_keys":true}`, 200)
	call("POST", "/key/generate", "master", `{"key":"sk-fixture-second"}`, 403)
	call("POST", "/key/regenerate", "master", `{"key":"sk-fixture-custom","new_key":"sk-fixture-second"}`, 403)
	call("POST", "/key/generate", "master", `{}`, 200)
}

