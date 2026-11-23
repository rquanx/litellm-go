package proxyconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestCLISSORequiresConfirmationAndConsumesTeamKeyOnce(t *testing.T) {
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Users = map[string]internalUser{"user": {ID: "user", Email: "login@example.invalid", Role: "internal_user"}}
	registry.state.Teams["team-a"] = team{ID: "team-a", Models: []string{"approved"}, Members: []teamMembership{{UserID: "user", Role: "user"}}}
	registry.state.Teams["team-b"] = team{ID: "team-b", Models: []string{"other"}, Members: []teamMembership{{UserID: "user", Role: "user"}}}
	base, _ := url.Parse("https://minato.example.invalid")
	server := &uiSSOServer{keys: registry, config: uiSSOConfig{base: base}, cli: map[string]*cliSSOFlow{
		"login": {expires: time.Now().Add(time.Minute), pollHash: keyDigest("poll-secret"), codeHash: keyDigest("1234ABCD")},
	}}
	const pollPath = "/sso/cli/poll/login"
	const completePath = "/sso/cli/complete/login"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sso/cli/complete/{login_id}", server.completeCLI)
	mux.HandleFunc("GET /sso/cli/poll/{login_id}", server.pollCLI)
	poll := func(path, secret string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Litellm-CLI-Poll-Secret", secret)
		mux.ServeHTTP(w, r)
		return w
	}
	if out := poll(pollPath, "wrong"); out.Code != http.StatusForbidden {
		t.Fatalf("unexpected invalid poll response: %d %s", out.Code, out.Body)
	}
	if out := poll(pollPath, "poll-secret"); out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"status":"pending"`) {
		t.Fatalf("unexpected pending response: %d %s", out.Code, out.Body)
	}
	page := httptest.NewRecorder()
	server.renderCLIConfirmation(page, "login", ssoIdentity{Subject: "subject", Email: "login@example.invalid", Verified: true})
	match := regexp.MustCompile(`name="browser_complete_token" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 || page.Code != http.StatusOK {
		t.Fatalf("verification form unavailable: %d %s", page.Code, page.Body)
	}
	complete := func(code, token string) int {
		form := url.Values{"user_code": {code}, "browser_complete_token": {token}}
		r := httptest.NewRequest(http.MethodPost, completePath, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if complete("1234-abcd", "wrong") != http.StatusBadRequest || complete("1234-abcd", match[1]) != http.StatusOK {
		t.Fatal("CLI confirmation did not require both verification values")
	}
	if out := poll(pollPath, "poll-secret"); !strings.Contains(out.Body.String(), `"requires_team_selection":true`) {
		t.Fatalf("team selection was skipped: %s", out.Body)
	}
	if out := poll(pollPath+"?team_id=denied", "poll-secret"); out.Code != http.StatusForbidden {
		t.Fatalf("unknown team was accepted: %s", out.Body)
	}
	out := poll(pollPath+"?team_id=team-a", "poll-secret")
	var result struct {
		Key string `json:"key"`
	}
	if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &result) != nil || result.Key == "" {
		t.Fatalf("team key unavailable: %d %s", out.Code, out.Body)
	}
	scope, allowed := registry.Authorize(result.Key)
	if !allowed || len(scope) != 1 || scope[0] != "approved" {
		t.Fatalf("invalid team key scope: %v", scope)
	}
	if out := poll(pollPath, "poll-secret"); out.Code != http.StatusNotFound {
		t.Fatal("CLI login could be redeemed again")
	}
}
