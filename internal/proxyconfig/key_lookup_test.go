package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyLookupsRespectMembershipAndRedactTokens(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Users = map[string]internalUser{"member": {ID: "member", Role: "internal_user"}}
	keys.state.Teams["member-team"] = team{ID: "member-team", Members: []teamMembership{{TeamID: "member-team", UserID: "member"}}}
	keys.state.Keys[keyDigest("member-key")] = virtualKey{UserID: "member", Alias: "personal"}
	keys.state.Keys[keyDigest("team-key")] = virtualKey{TeamID: "member-team", Alias: "common"}
	digest := keyDigest("unrelated-key")
	keys.state.Keys[digest] = virtualKey{Alias: "private"}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(method, path, token, body string, status int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("got %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	page := call("GET", "/key/aliases?size=1&page=2", "member-key", "", 200)
	if !strings.Contains(page, `"aliases":["personal"]`) || !strings.Contains(page, `"total_count":2`) {
		t.Fatalf("unexpected aliases page: %s", page)
	}
	scoped := call("POST", "/v2/key/info", "member-key", `{"key_aliases":["private","common"]}`, 200)
	if strings.Contains(scoped, "private") || !strings.Contains(scoped, `"key_alias":"common"`) {
		t.Fatalf("unexpected scoped key information: %s", scoped)
	}
	call("GET", "/key/info?key="+digest, "member-key", "", 404)
	self := call("GET", "/key/info", "member-key", "", 200)
	if !strings.Contains(self, `"key_alias":"personal"`) {
		t.Fatalf("unexpected self key information: %s", self)
	}
	result := call("POST", "/v2/key/info", "master", `{"key_aliases":["private"]}`, 200)
	if !strings.Contains(result, `"key_alias":"private"`) || strings.Contains(result, digest) || strings.Contains(result, "unrelated-key") {
		t.Fatalf("unexpected key information: %s", result)
	}
	call("POST", "/key/service-account/generate", "master", `{"key_alias":"missing-team"}`, 400)
	call("POST", "/key/service-account/generate", "master",
		`{"key_alias":"service","team_id":"member-team","user_id":"member"}`, 200)
	service := call("POST", "/v2/key/info", "master", `{"key_aliases":["service"]}`, 200)
	if !strings.Contains(service, `"team_id":"member-team"`) || strings.Contains(service, `"user_id":`) {
		t.Fatalf("expected a team key independent of the user: %s", service)
	}
}
