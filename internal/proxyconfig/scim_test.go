package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSCIMDiscoveryAndReadResources(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = map[string]internalUser{
		"alice": {ID: "alice", Email: "alice@example.com", Alias: "Alice", Metadata: map[string]any{}},
	}
	keys.state.Teams = map[string]team{
		"team-1": {ID: "team-1", Alias: "Engineering", Members: []teamMembership{{UserID: "alice", TeamID: "team-1"}}},
	}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(path string, want int) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want || !strings.Contains(w.Header().Get("Content-Type"), "application/scim+json") {
			t.Fatalf("%s returned %d with %q: %s", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		return w.Body.String()
	}
	if body := call("/scim/v2/ServiceProviderConfig", http.StatusOK); !strings.Contains(body, `"patch":{"supported":false}`) {
		t.Fatalf("unexpected service provider config: %s", body)
	}
	if body := call("/scim/v2/ResourceTypes", http.StatusOK); !strings.Contains(body, `"id":"User"`) {
		t.Fatalf("missing User ResourceType: %s", body)
	}
	if body := call(`/scim/v2/Users?filter=userName%20eq%20%22alice%22`, http.StatusOK); !strings.Contains(body, `"userName":"alice"`) {
		t.Fatalf("missing SCIM user: %s", body)
	}
	if body := call("/scim/v2/Groups/team-1", http.StatusOK); !strings.Contains(body, `"displayName":"Engineering"`) || !strings.Contains(body, `"value":"alice"`) {
		t.Fatalf("missing SCIM group: %s", body)
	}
	if body := call("/scim/v2/Users/missing", http.StatusNotFound); !strings.Contains(body, `"status":"404"`) {
		t.Fatalf("unexpected SCIM error: %s", body)
	}
}

func TestSCIMLifecycleMutationsUpdateStateAndKeys(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = map[string]internalUser{}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(method, path, body string, want int) string {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	call(http.MethodPost, "/scim/v2/Users", `{"userName":"alice","externalId":"idp-alice","displayName":"Alice","emails":[{"value":"alice@example.com"}]}`, http.StatusCreated)
	call(http.MethodPost, "/scim/v2/Groups", `{"id":"engineering","externalId":"idp-engineering","displayName":"Engineering","members":[{"value":"alice"}]}`, http.StatusCreated)
	call(http.MethodPut, "/scim/v2/Users/alice", `{"displayName":"Alice Updated","active":false}`, http.StatusOK)
	if keys.state.Users["alice"].Metadata["scim_active"] != false {
		t.Fatal("SCIM active state was not persisted")
	}
	if body := call(http.MethodGet, "/scim/v2/Groups?filter=externalId%20eq%20%22idp-engineering%22", "", http.StatusOK); !strings.Contains(body, `"externalId":"idp-engineering"`) {
		t.Fatalf("externalId group filter did not return the group: %s", body)
	}
	if body := call(http.MethodGet, "/scim/v2/Users?filter=externalId%20eq%20%22idp-alice%22", "", http.StatusOK); !strings.Contains(body, `"externalId":"idp-alice"`) {
		t.Fatalf("externalId filter did not return the user: %s", body)
	}
	call(http.MethodDelete, "/scim/v2/Groups/engineering", "", http.StatusNoContent)
	call(http.MethodDelete, "/scim/v2/Users/alice", "", http.StatusNoContent)
	if _, exists := keys.state.Users["alice"]; exists {
		t.Fatal("SCIM user was not deleted")
	}
}

func TestSCIMPatchUserAndGroup(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	keys.state.Users = map[string]internalUser{"alice": {ID: "alice", Email: "alice@example.com", Metadata: map[string]any{}}}
	keys.state.Teams = map[string]team{"engineering": {ID: "engineering", Members: []teamMembership{{TeamID: "engineering", UserID: "alice", Role: "user"}}}}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(path, body string) string {
		req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body)); req.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder(); handler.ServeHTTP(w, req); if w.Code != http.StatusOK { t.Fatalf("PATCH %s returned %d: %s", path, w.Code, w.Body.String()) }; return w.Body.String()
	}
	call("/scim/v2/Users/alice", `{"Operations":[{"op":"replace","path":"active","value":false},{"op":"replace","path":"displayName","value":"Alice Updated"}]}`)
	if keys.state.Users["alice"].Metadata["scim_active"] != false || keys.state.Users["alice"].Alias != "Alice Updated" { t.Fatal("user patch was not applied") }
	call("/scim/v2/Groups/engineering", `{"Operations":[{"op":"remove","path":"members[value eq \"alice\"]"}]}`)
	if len(keys.state.Teams["engineering"].Members) != 0 { t.Fatal("group member patch was not applied") }
}

func TestSCIMMutationUnauthorizedUsesSCIMError(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	handler := keys.Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodPost, "/scim/v2/Users", strings.NewReader(`{"userName":"alice"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder(); handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("Content-Type"), "application/scim+json") || !strings.Contains(w.Body.String(), `"status":"401"`) {
		t.Fatalf("unexpected SCIM auth error %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}
