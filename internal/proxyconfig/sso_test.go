package proxyconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func TestGenericUISSOIssuesScopedKeyAfterPKCE(t *testing.T) {
	var challenge string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			id, secret, ok := r.BasicAuth()
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || id != "fixture" || secret != "fixture-secret" || r.Form.Get("code") != "fixture-code" ||
				base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
				t.Error("invalid PKCE token exchange")
			}
			w.Write([]byte(`{"access_token":"fixture-access","token_type":"Bearer"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				t.Error("missing SSO access token")
			}
			w.Write([]byte(`{"sub":"fixture-subject","email":"login@example.invalid","email_verified":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("MICROSOFT_CLIENT_ID", "")
	t.Setenv("GENERIC_INCLUDE_CLIENT_ID", "")
	t.Setenv("GENERIC_CLIENT_ID", "fixture")
	t.Setenv("GENERIC_CLIENT_SECRET", "fixture-secret")
	t.Setenv("GENERIC_AUTHORIZATION_ENDPOINT", upstream.URL+"/authorize")
	t.Setenv("GENERIC_TOKEN_ENDPOINT", upstream.URL+"/token")
	t.Setenv("GENERIC_USERINFO_ENDPOINT", upstream.URL+"/userinfo")
	t.Setenv("PROXY_BASE_URL", "https://minato.example.invalid")
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Users = map[string]internalUser{"user": {ID: "user", Email: "login@example.invalid", Role: "internal_user", Models: []string{"approved"}}}
	handler := registry.Handler(http.NotFoundHandler())
	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/sso/key/generate", nil))
	redirect, err := url.Parse(start.Header().Get("Location"))
	if err != nil || start.Code != http.StatusFound || !strings.HasSuffix(redirect.Path, "/authorize") {
		t.Fatalf("SSO start returned %d: %s", start.Code, start.Body)
	}
	params := redirect.Query()
	challenge = params.Get("code_challenge")
	cookies := start.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || params.Get("state") == "" || challenge == "" || params.Get("code_challenge_method") != "S256" {
		t.Fatal("missing protected SSO state or PKCE configuration")
	}
	callback := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/sso/callback?code=fixture-code&state="+url.QueryEscape(params.Get("state")), nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if callback(nil).Code != http.StatusUnauthorized {
		t.Fatal("callback without cookie was accepted")
	}
	completed := callback(cookies[0])
	if completed.Code != http.StatusSeeOther || !strings.HasSuffix(completed.Header().Get("Location"), "/ui?login=success") {
		t.Fatalf("callback returned %d: %s", completed.Code, completed.Body)
	}
	for _, cookie := range completed.Result().Cookies() {
		if cookie.Name != "token" {
			continue
		}
		claims := jwt.MapClaims{}
		_, err := jwt.ParseWithClaims(cookie.Value, claims, func(*jwt.Token) (any, error) { return []byte("fixture-master"), nil }, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil || !cookie.Secure || claims["user_id"] != "user" {
			t.Fatal("invalid signed UI session")
		}
		key, _ := claims["key"].(string)
		scope, allowed := registry.Authorize(key)
		if !allowed || len(scope) != 1 || scope[0] != "approved" {
			t.Fatalf("SSO key has wrong scope: %v", scope)
		}
		if callback(cookies[0]).Code != http.StatusUnauthorized {
			t.Fatal("SSO state was reusable")
		}
		return
	}
	t.Fatal("missing UI session cookie")
}
