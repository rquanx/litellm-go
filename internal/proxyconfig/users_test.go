package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAvailableUsersAliasUsesScopedSearch(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	keys.state.Users = map[string]internalUser{
		"alice": {ID: "alice", Email: "alice@example.com", Role: "internal_user"},
		"bob": {ID: "bob", Email: "bob@example.com", Role: "internal_user"},
	}
	handler := keys.Handler(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/user/available_users?user_email=alice", nil)
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "alice@example.com") || strings.Contains(w.Body.String(), "bob@example.com") {
		t.Fatalf("unexpected available users response %d: %s", w.Code, w.Body.String())
	}
}
