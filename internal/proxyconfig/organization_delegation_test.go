package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func organizationFixture(t *testing.T) *KeyRegistry {
	t.Helper()
	keys, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = make(map[string]internalUser)
	keys.state.Organizations = make(map[string]organization)
	for _, id := range []string{"admin", "member", "outsider"} {
		keys.state.Users[id] = internalUser{ID: id, Role: "internal_user"}
		keys.state.Keys[keyDigest(id)] = virtualKey{UserID: id}
	}
	keys.state.Organizations["delegation-owned"] = organization{
		ID: "delegation-owned", Alias: "Owned", Metadata: map[string]any{"keep": true},
		Members: []organizationMember{{UserID: "admin", Role: "org_admin"}, {UserID: "member", Role: "internal_user"}},
	}
	keys.state.Organizations["delegation-other"] = organization{ID: "delegation-other", Alias: "Other"}
	keys.state.Teams["owned-team"] = team{ID: "owned-team", OrgID: "delegation-owned"}
	keys.state.Teams["other-team"] = team{ID: "other-team", OrgID: "delegation-other"}
	keys.state.Keys[keyDigest("team-only")] = virtualKey{TeamID: "owned-team"}
	return keys
}

func organizationRequest(t *testing.T, handler http.Handler, method, path, token, body string, status int) string {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != status {
		t.Fatalf("%s %s returned %d, want %d: %s", method, path, res.Code, status, res.Body)
	}
	return res.Body.String()
}

func TestOrganizationDelegation(t *testing.T) {
	keys := organizationFixture(t)
	handler := keys.Handler(http.NotFoundHandler())
	request := func(method, path, token, body string, status int) string {
		t.Helper()
		return organizationRequest(t, handler, method, path, token, body, status)
	}
	const owned = "/organization/info?organization_id=delegation-owned"
	request("GET", owned, "invalid", "", 401)
	request("GET", owned, "admin", "", 200)
	request("GET", owned, "member", "", 403)
	request("GET", "/organization/info?organization_id=delegation-other", "admin", "", 403)
	request("POST", "/organization/info", "admin", `{"organizations":["delegation-owned"]}`, 200)
	request("POST", "/organization/info", "admin", `{"organizations":["delegation-owned","delegation-other"]}`, 403)
	list := request("GET", "/organization/list", "member", "", 200)
	if !strings.Contains(list, "delegation-owned") || strings.Contains(list, "delegation-other") {
		t.Fatalf("incorrect membership list: %s", list)
	}
	if list := request("GET", "/organization/list?org_id=delegation-other", "admin", "", 200); strings.TrimSpace(list) != "[]" {
		t.Fatalf("cross-organization list returned %s", list)
	}
	body := request("PATCH", "/organization/update", "admin", `{"organization_id":"delegation-owned","metadata":{"added":true}}`, 200)
	if !strings.Contains(body, `"keep":true`) || !strings.Contains(body, `"added":true`) {
		t.Fatalf("organization metadata was not merged: %s", body)
	}
	request("PATCH", "/organization/update", "admin", `{"organization_id":"delegation-other","organization_alias":"changed"}`, 403)
	request("POST", "/organization/member_add", "admin", `{"organization_id":"delegation-other","member":{"user_id":"outsider","role":"org_admin"}}`, 403)
	request("POST", "/organization/member_add", "admin", `{"organization_id":"delegation-owned","member":{"user_id":"outsider","role":"internal_user"}}`, 200)
	request("PATCH", "/organization/member_update", "member", `{"organization_id":"delegation-owned","user_id":"member","role":"org_admin"}`, 403)
	request("PATCH", "/organization/member_update", "admin", `{"organization_id":"delegation-owned","user_id":"outsider","role":"org_admin"}`, 200)
	request("GET", owned, "outsider", "", 200)
	request("DELETE", "/organization/member_delete", "admin", `{"organization_id":"delegation-owned","user_id":"outsider"}`, 200)
	request("GET", owned, "outsider", "", 403)
	request("DELETE", "/organization/delete", "admin", `{"organization_ids":["delegation-owned"]}`, 401)
	request("GET", "/team/info?team_id=owned-team", "admin", "", 200)
	request("GET", "/team/info?team_id=owned-team", "team-only", "", 200)
	request("GET", "/team/info?team_id=other-team", "team-only", "", 404)
	request("GET", "/team/info?team_id=other-team", "admin", "", 404)
	request("GET", "/user/list", "member", "", 403)
	request("GET", "/user/list?organization_ids=delegation-other", "admin", "", 403)
	users := request("GET", "/user/list?organization_ids=delegation-owned,delegation-other", "admin", "", 200)
	if !strings.Contains(users, `"user_id":"member"`) || strings.Contains(users, `"user_id":"outsider"`) {
		t.Fatalf("delegated user list exceeded organization scope: %s", users)
	}
	request("GET", "/v2/team/list?organization_id=delegation-other", "admin", "", 403)
	list = request("GET", "/v2/team/list", "admin", "", 200)
	if !strings.Contains(list, "owned-team") || strings.Contains(list, "other-team") {
		t.Fatalf("incorrect delegated team list: %s", list)
	}
	request("POST", "/team/member_add", "admin", `{"team_id":"owned-team","member":{"user_id":"member","role":"user"}}`, 200)
	request("POST", "/team/member_add", "admin", `{"team_id":"other-team","member":{"user_id":"member","role":"admin"}}`, 403)
	request("POST", "/team/model/add", "member", `{"team_id":"owned-team","models":["one"]}`, 403)
	request("POST", "/team/model/add", "admin", `{"team_id":"other-team","models":["one"]}`, 403)
	body = request("POST", "/team/model/add", "admin", `{"team_id":"owned-team","models":["one","one","two"]}`, 200)
	if !strings.Contains(body, `"models":["all-proxy-models","one","two"]`) {
		t.Fatalf("team did not retain unrestricted access: %s", body)
	}
	body = request("POST", "/team/model/delete", "admin", `{"team_id":"owned-team","models":["all-proxy-models","two"]}`, 200)
	if !strings.Contains(body, `"models":["one"]`) {
		t.Fatalf("team model removal returned %s", body)
	}
}

func TestOrganizationActivityDelegation(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys := organizationFixture(t)
	log, err := OpenSpendLog(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().AddDate(-2, 0, 0)
	for id, tokens := range map[string]int{"delegation-owned": 12, "delegation-other": 50} {
		log.Record(runtime.InferenceEvent{
			ID: newEntityID(), Model: "organization-delegation-fixture", Total: tokens, Status: "success",
			SpendAttribution: &runtime.SpendAttribution{OrgID: id},
			StartTime: float64(day.Unix()), EndTime: float64(day.Unix() + 1),
		})
	}
	handler := log.Handler(http.NotFoundHandler())
	date := day.Format(time.DateOnly)
	path := "/organization/daily/activity?model=organization-delegation-fixture&start_date=" + date + "&end_date=" + date
	for token, want := range map[string]int{"admin": 12, "member": 0, "fixture-master": 62} {
		body := organizationRequest(t, handler, "GET", path, token, "", 200)
		var response struct {
			Metadata struct {
				Tokens int `json:"total_tokens"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil || response.Metadata.Tokens != want {
			t.Fatalf("usage for %s returned %s, want %d tokens", token, body, want)
		}
		if token == "admin" && (!strings.Contains(body, `"organization_alias":"Owned"`) || strings.Contains(body, "delegation-other")) {
			t.Fatalf("organization breakdown exceeded scope: %s", body)
		}
	}
	organizationRequest(t, handler, "GET", path+"&organization_ids=delegation-owned,delegation-other", "admin", "", 403)
	organizationRequest(t, handler, "GET", path+"&exclude_organization_ids=delegation-other", "invalid", "", 401)
}

func TestBulkKeyUpdateCommitsValidItems(t *testing.T) {
	keys := organizationFixture(t)
	handler := keys.Handler(http.NotFoundHandler())
	const path = "/key/bulk_update"
	const body = `{"keys":[{"key":"admin","tags":["example"]},{"key":"member","team_id":"nonexistent"},{"key":"outsider","team_id":"owned-team"},{"key":"missing"}]}`
	organizationRequest(t, handler, "POST", path, "member", body, 403)
	response := organizationRequest(t, handler, "POST", path, "fixture-master", body, 200)
	var result struct {
		Count   int             `json:"total_requested"`
		Success []bulkKeyResult `json:"successful_updates"`
		Failure []bulkKeyResult `json:"failed_updates"`
	}
	if err := json.Unmarshal([]byte(response), &result); err != nil || result.Count != 4 ||
		len(result.Success) != 2 || len(result.Failure) != 2 || result.Failure[0].Info == nil ||
		result.Failure[1].Info != nil {
		t.Fatalf("unexpected batch response: %s", response)
	}
	if keys.state.Keys[keyDigest("outsider")].TeamID != "owned-team" ||
		keys.state.Keys[keyDigest("member")].TeamID != "" ||
		!strings.Contains(response, `"tags":["example"]`) {
		t.Fatalf("batch did not preserve successful updates: %s", response)
	}
}

func TestUserCreationAssignsMembershipsAtomically(t *testing.T) {
	keys := organizationFixture(t)
	handler := keys.Handler(http.NotFoundHandler())
	const master = "fixture-master"
	organizationRequest(t, handler, "POST", "/user/new", master,
		`{"user_id":"new-user","organizations":["delegation-owned"],"team_id":"owned-team","duration":"invalid"}`, 400)
	if keys.state.Users["new-user"].ID != "" || memberIndex(keys.state.Teams["owned-team"], "new-user") >= 0 {
		t.Fatal("failed credential creation left user state behind")
	}
	body := organizationRequest(t, handler, "POST", "/user/new", master,
		`{"user_id":"new-user","organizations":["delegation-owned"],"team_id":"owned-team","key_alias":"bootstrap","duration":"1h"}`, 200)
	var user struct{ Key string }
	if err := json.Unmarshal([]byte(body), &user); err != nil || user.Key == "" {
		t.Fatal("user creation omitted its key")
	}
	key := keys.state.Keys[keyDigest(user.Key)]
	if key.TeamID != "owned-team" || key.Alias != "bootstrap" || key.Expires == nil ||
		memberIndex(keys.state.Teams["owned-team"], "new-user") < 0 ||
		organizationRole(keys.state.Organizations["delegation-owned"], "new-user") != "internal_user" {
		t.Fatal("user, memberships and credential did not commit together")
	}
}

func TestBulkUserUpdateSupportsPartialAndAllUpdates(t *testing.T) {
	keys := organizationFixture(t)
	handler := keys.Handler(http.NotFoundHandler())
	const path = "/user/bulk_update"
	const master = "fixture-master"
	body := organizationRequest(t, handler, "POST", path, master,
		`{"users":[{"user_id":"member","rpm_limit":2},{"user_id":"unknown","rpm_limit":7},{"user_id":"admin","rpm_limit":3}]}`, 200)
	if !strings.Contains(body, `"successful_updates":2`) || !strings.Contains(body, `"failed_updates":1`) {
		t.Fatalf("unexpected partial results: %s", body)
	}
	organizationRequest(t, handler, "POST", path, master, `{"all_users":true,"user_updates":{"rpm_limit":-1}}`, 400)
	if *keys.state.Users["member"].RPM != 2 || *keys.state.Users["admin"].RPM != 3 {
		t.Fatal("invalid bulk update changed prepared users")
	}
	organizationRequest(t, handler, "POST", path, master, `{"all_users":true,"user_updates":{"rpm_limit":4}}`, 200)
	for _, user := range keys.state.Users {
		if user.RPM == nil || *user.RPM != 4 {
			t.Fatalf("user %s was not updated", user.ID)
		}
	}
}

func TestBulkTeamMembershipScopesAndCommitsAtomically(t *testing.T) {
	keys := organizationFixture(t)
	handler := keys.Handler(http.NotFoundHandler())
	const path = "/team/bulk_member_add"
	organizationRequest(t, handler, "POST", path, "admin", `{"team_id":"owned-team","all_users":true}`, 403)
	organizationRequest(t, handler, "POST", path, "admin", `{"team_id":"other-team","members":[{"user_id":"member","role":"user"}]}`, 403)
	body := organizationRequest(t, handler, "POST", path, "admin",
		`{"team_id":"owned-team","members":[{"user_id":"new-member","role":"user"},{"user_id":"member","role":"invalid"}]}`, 200)
	if !strings.Contains(body, `"failed_additions":2`) || keys.state.Users["new-member"].ID != "" {
		t.Fatalf("invalid batch modified membership: %s", body)
	}
	body = organizationRequest(t, handler, "POST", path, "admin",
		`{"team_id":"owned-team","members":[{"user_id":"new-member","role":"user"},{"user_id":"member","role":"user"}]}`, 200)
	if !strings.Contains(body, `"successful_additions":2`) || memberIndex(keys.state.Teams["owned-team"], "member") < 0 {
		t.Fatalf("valid batch did not add its members: %s", body)
	}
	keys.SetSSOUserDefaults(&SSOUserDefaults{AvailableTeams: []string{"owned-team", "other-team"}}, "")
	available := organizationRequest(t, handler, "GET", "/team/available", "member", "", 200)
	if !strings.Contains(available, "other-team") || strings.Contains(available, "owned-team") {
		t.Fatalf("available teams included an existing membership: %s", available)
	}
}

func TestOrganizationAdminCannotDeleteSharedUsers(t *testing.T) {
	keys := organizationFixture(t)
	org := keys.state.Organizations["delegation-other"]
	org.Members = []organizationMember{{UserID: "member", Role: "internal_user"}}
	keys.state.Organizations[org.ID] = org
	handler := keys.Handler(http.NotFoundHandler())
	const path = "/user/delete"
	organizationRequest(t, handler, "POST", path, "admin",
		`{"organization_id":"delegation-owned","user_ids":["admin","member"]}`, 403)
	if keys.state.Users["admin"].ID == "" || keys.state.Users["member"].ID == "" {
		t.Fatal("a rejected delete changed user records")
	}
	organizationRequest(t, handler, "POST", path, "admin", `{"user_ids":["outsider"]}`, 403)
	org.Members = nil
	keys.state.Organizations[org.ID] = org
	organizationRequest(t, handler, "POST", path, "admin",
		`{"organization_id":"delegation-owned","user_ids":["member"]}`, 200)
	if keys.state.Users["member"].ID != "" || organizationRole(keys.state.Organizations["delegation-owned"], "member") != "" {
		t.Fatal("authorized deletion left user or membership data")
	}
	if deleted := keys.state.DeletedKeys[keyDigest("member")]; deleted.UserID != "member" || deleted.DeletedBy != "admin" {
		t.Fatal("user deletion omitted the revoked key's history")
	}
}

func TestDelegatedUserCreationBindsCredentialsToOrganization(t *testing.T) {
	keys := organizationFixture(t)
	org := keys.state.Organizations["delegation-owned"]
	org.Models = []string{"restricted-model"}
	keys.state.Organizations[org.ID] = org
	handler := keys.Handler(http.NotFoundHandler())
	const path = "/user/new"
	organizationRequest(t, handler, "POST", path, "admin", `{"user_id":"outside","organizations":["delegation-other"]}`, 403)
	organizationRequest(t, handler, "POST", path, "admin",
		`{"user_id":"outside","organizations":["delegation-owned"],"user_role":"proxy_admin_viewer"}`, 403)
	body := organizationRequest(t, handler, "POST", path, "admin", `{"user_id":"scoped","organizations":["delegation-owned"]}`, 200)
	var created struct{ Key string }
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Key == "" {
		t.Fatalf("invalid delegated response: %s", body)
	}
	if models, ok := keys.Authorize(created.Key); !ok || len(models) != 1 || models[0] != "restricted-model" {
		t.Fatalf("delegated key escaped its organization: %v %v", models, ok)
	}
	organizationRequest(t, handler, "DELETE", "/organization/member_delete", "admin",
		`{"organization_id":"delegation-owned","user_id":"scoped"}`, 200)
	if _, ok := keys.Authorize(created.Key); ok {
		t.Fatal("removed organization membership kept its credentials")
	}
}

func TestKeyListLimitsTeamAndPersonalVisibility(t *testing.T) {
	keys := organizationFixture(t)
	team := keys.state.Teams["owned-team"]
	team.Members = []teamMembership{{UserID: "admin", Role: "admin"}, {UserID: "member", Role: "user"}}
	keys.state.Teams[team.ID] = team
	keys.state.Keys[keyDigest("personal")] = virtualKey{UserID: "member", Alias: "member-personal"}
	keys.state.Keys[keyDigest("team-owned")] = virtualKey{UserID: "admin", TeamID: team.ID, Alias: "another-owner"}
	keys.state.Keys[keyDigest("team-only")] = virtualKey{TeamID: team.ID, Alias: "team-service"}
	handler := keys.Handler(http.NotFoundHandler())
	base := "/key/list?return_full_object=true"
	body := organizationRequest(t, handler, "GET", base, "member", "", 200)
	if !strings.Contains(body, "member-personal") || strings.Contains(body, "another-owner") || strings.Contains(body, "team-service") {
		t.Fatalf("personal list exceeded the user scope: %s", body)
	}
	body = organizationRequest(t, handler, "GET", base+"&include_team_keys=true", "member", "", 200)
	if !strings.Contains(body, "team-service") || strings.Contains(body, "another-owner") {
		t.Fatalf("unexpected regular-member list: %s", body)
	}
	body = organizationRequest(t, handler, "GET", base+"&include_team_keys=true", "admin", "", 200)
	if !strings.Contains(body, "another-owner") || !strings.Contains(body, "team-service") || strings.Contains(body, "member-personal") {
		t.Fatalf("unexpected team administrator list: %s", body)
	}
	organizationRequest(t, handler, "POST", "/team/permissions_update", "member", `{"team_id":"owned-team","team_member_permissions":["/key/list"]}`, 403)
	organizationRequest(t, handler, "POST", "/team/permissions_update", "admin", `{"team_id":"owned-team","team_member_permissions":["/key/list"]}`, 200)
	body = organizationRequest(t, handler, "GET", base+"&include_team_keys=true&expand=user", "member", "", 200)
	if !strings.Contains(body, "another-owner") || !strings.Contains(body, `"user":{`) {
		t.Fatalf("team grant or user expansion was not reflected: %s", body)
	}
	organizationRequest(t, handler, "POST", "/team/permissions_bulk_update", "fixture-master",
		`{"team_ids":["owned-team"],"permissions":["/team/daily/activity"]}`, 200)
	body = organizationRequest(t, handler, "GET", "/team/permissions_list?team_id=owned-team", "admin", "", 200)
	if !strings.Contains(body, `"team_member_permissions":["/key/list","/team/daily/activity"]`) {
		t.Fatalf("bulk permission update did not merge grants: %s", body)
	}
	organizationRequest(t, handler, "GET", base+"&team_id=other-team", "member", "", 403)
	organizationRequest(t, handler, "POST", "/key/delete", "fixture-master", `{"key_aliases":["member-personal"]}`, 200)
	body = organizationRequest(t, handler, "GET", base+"&status=deleted", "member", "", 200)
	if !strings.Contains(body, "member-personal") || !strings.Contains(body, `"deleted_at"`) {
		t.Fatalf("deleted key history omitted the revoked key: %s", body)
	}
	if _, ok := keys.Authorize("personal"); ok {
		t.Fatal("deleted key history kept the key valid")
	}
	body = organizationRequest(t, handler, "GET", base+"&status=deleted", "outsider", "", 200)
	if strings.Contains(body, "member-personal") {
		t.Fatalf("deleted key history exceeded caller scope: %s", body)
	}
}
