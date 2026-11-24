package proxyconfig

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSSORoleMappingIssuesAdminViewerCredential(t *testing.T) {
	keys := organizationFixture(t)
	server := uiSSOServer{keys: keys}
	cfg := uiSSOConfig{roleClaim: "groups",
		roleGroups: map[string][]string{adminViewerRole: {"operators"}, "internal_user": {"employees"}}}
	role := cfg.userRole(map[string]any{"groups": []any{"employees", "operators"}})
	w := httptest.NewRecorder()
	signed, _, ok := server.issueSession(w, ssoIdentity{Subject: "admin-viewer", Email: "viewer@example.test", Verified: true, Role: role})
	if ok {
		t.Fatal("SSO created an account without provisioning")
	}
	user := keys.state.Users["admin"]
	user.Email = "viewer@example.test"
	keys.state.Users[user.ID] = user
	w = httptest.NewRecorder()
	signed, _, ok = server.issueSession(w, ssoIdentity{Subject: "admin-viewer", Email: user.Email, Verified: true, Role: role})
	if !ok {
		t.Fatalf("SSO session failed: %s", w.Body)
	}
	token, err := jwt.Parse(signed, func(*jwt.Token) (any, error) { return []byte(keys.master), nil },
		jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		t.Fatalf("invalid SSO session: %v", err)
	}
	claims := token.Claims.(jwt.MapClaims)
	key, _ := claims["key"].(string)
	if key == "" || claims["user_role"] != adminViewerRole {
		t.Fatal("SSO did not issue an administrator viewer credential")
	}
	handler := keys.Handler(http.NotFoundHandler())
	organizationRequest(t, handler, "GET", "/organization/list", key, "", 200)
	organizationRequest(t, handler, "POST", "/key/generate", key, `{}`, 401)
}

func TestPostgresBrowserSSOStateIsSingleUseAcrossReplicas(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	primary, err := openPostgresBrowserSSO(db, "master")
	if err != nil {
		t.Fatal(err)
	}
	replica, err := openPostgresBrowserSSO(db, "master")
	if err != nil {
		t.Fatal(err)
	}
	state := "sso-fixture-" + newEntityID()
	ctx := context.Background()
	if err := primary.save(ctx, "fixture-provider", state, uiSSOFlow{verifier: "fixture-verifier", expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := db.QueryRow(`SELECT verifier FROM `+browserSSOTable+` WHERE state_digest=$1`, keyDigest(state)).Scan(&encrypted); err != nil ||
		strings.Contains(encrypted, "fixture-verifier") {
		t.Fatal("SSO verifier was not durably encrypted")
	}
	if _, err := replica.consume(ctx, "different-provider", state); err == nil {
		t.Fatal("wrong SSO provider was allowed to consume state")
	}
	if flow, err := replica.consume(ctx, "fixture-provider", state); err != nil || flow.verifier != "fixture-verifier" {
		t.Fatalf("replica could not consume the authorization state: %v", err)
	}
	if _, err := primary.consume(ctx, "fixture-provider", state); err == nil {
		t.Fatal("browser SSO state was consumed twice")
	}
}

func TestPostgresSSOMappingsUpdatePreservesProviderSettings(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE ` + ssoConfigTable + ` (
		id text PRIMARY KEY,sso_settings jsonb,updated_at timestamp DEFAULT CURRENT_TIMESTAMP
	);
	INSERT INTO ` + ssoConfigTable + ` VALUES ('sso_config', '{"generic_client_secret":"stored-secret"}')`); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadPostgresKeys("master", db)
	if err != nil {
		t.Fatal(err)
	}
	handler := keys.Handler(http.NotFoundHandler())
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	patch := `{"role_mappings":{"provider":"generic","group_claim":"groups","roles":{"internal_user_viewer":["readers"]}},
		"team_mappings":{"team_ids_jwt_field":"identity.teams"}}`
	if response := call(http.MethodPatch, "/update/sso_settings", "master", patch); response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "stored-secret") {
		t.Fatalf("unexpected mapping update %d %s", response.Code, response.Body)
	}
	var secret string
	if err := db.QueryRow(`SELECT sso_settings->>'generic_client_secret' FROM ` + ssoConfigTable).Scan(&secret); err != nil || secret != "stored-secret" {
		t.Fatal("mapping update changed existing provider settings")
	}
	server := uiSSOServer{keys: keys}
	cfg, teamClaim, err := server.effectiveClaimMappings(context.Background(), uiSSOConfig{provider: "generic"})
	if err != nil || teamClaim != "identity.teams" || cfg.userRole(map[string]any{"groups": []any{"readers"}}) != "internal_user_viewer" {
		t.Fatalf("stored mappings were not applied: %v %s %+v", err, teamClaim, cfg.roleGroups)
	}
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("MICROSOFT_CLIENT_ID", "")
	t.Setenv("GENERIC_ROLE_MAPPINGS_ROLES", "")
	patch = `{"generic_client_id":"fixture","generic_client_secret":"fixture-oauth-secret","generic_authorization_endpoint":"https://auth.example.invalid/authorize",
		"generic_token_endpoint":"https://auth.example.invalid/token","generic_userinfo_endpoint":"https://auth.example.invalid/userinfo","proxy_base_url":"https://proxy.example.invalid"}`
	if response := call(http.MethodPatch, "/update/sso_settings", "master", patch); response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "fixture-oauth-secret") {
		t.Fatalf("unexpected provider update %d %s", response.Code, response.Body)
	}
	if err := db.QueryRow(`SELECT sso_settings->>'generic_client_secret' FROM ` + ssoConfigTable).Scan(&secret); err != nil ||
		!strings.HasPrefix(secret, "minato-encrypted:") || strings.Contains(secret, "fixture-oauth-secret") {
		t.Fatal("updated OAuth client secret was not encrypted")
	}
	result := call(http.MethodGet, "/sso/key/generate", "", "")
	if result.Code != http.StatusFound || !strings.HasPrefix(result.Header().Get("Location"), "https://auth.example.invalid/authorize?") {
		t.Fatalf("database provider settings were not used: %d %s", result.Code, result.Body)
	}
	result = call(http.MethodGet, "/get/sso_settings", "master", "")
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"generic_client_secret":"********"`) ||
		strings.Contains(result.Body.String(), "fixture-oauth-secret") {
		t.Fatalf("settings did not redact the secret: %d %s", result.Code, result.Body)
	}
}

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
			w.Write([]byte(`{"identity":{"id":"fixture-subject","mail":"login@example.invalid"},"email_verified":true,"directory":{"teams":["sso-team","missing-team"]}}`))
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
	t.Setenv("GENERIC_USER_ID_ATTRIBUTE", "identity.id")
	t.Setenv("GENERIC_USER_EMAIL_ATTRIBUTE", "identity.mail")
	t.Setenv("GENERIC_AUTHORIZATION_ENDPOINT", upstream.URL+"/authorize")
	t.Setenv("GENERIC_TOKEN_ENDPOINT", upstream.URL+"/token")
	t.Setenv("GENERIC_USERINFO_ENDPOINT", upstream.URL+"/userinfo")
	t.Setenv("PROXY_BASE_URL", "https://minato.example.invalid")
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Users = map[string]internalUser{"user": {ID: "user", Email: "login@example.invalid", Role: "internal_user", Models: []string{"approved"}}}
	registry.state.Teams["sso-team"] = team{ID: "sso-team"}
	registry.SetSSOUserDefaults(nil, "directory.teams")
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
	if memberIndex(registry.state.Teams["sso-team"], "user") < 0 || registry.state.Teams["missing-team"].ID != "" {
		t.Fatal("SSO team claims did not add the user only to configured teams")
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

func TestSSOMappedViewerCanReadOwnProfileWithoutInference(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.SetSSOUserDefaults(&SSOUserDefaults{Role: "internal_user"}, "")
	issuer := &uiSSOServer{keys: keys, config: uiSSOConfig{provider: "generic", roleClaim: "groups", roleGroups: map[string][]string{
		"internal_user_viewer": {"readers"},
	}}}
	identity := ssoIdentity{Subject: "mapped", Email: "viewer@example.invalid", Verified: true, Role: issuer.config.userRole(map[string]any{"groups": []any{"readers"}})}
	signed, _, ok := issuer.issueSession(httptest.NewRecorder(), identity)
	if !ok {
		t.Fatal("viewer SSO session was rejected")
	}
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) { return []byte("master"), nil }); err != nil {
		t.Fatal(err)
	}
	token, _ := claims["key"].(string)
	if token == "" || claims["user_role"] != "internal_user_viewer" {
		t.Fatal("viewer session did not identify its role")
	}
	if _, authorized := keys.Authorize(token); authorized {
		t.Fatal("viewer token was authorized for inference")
	}
	handler := keys.Handler(http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodGet, "/user/info", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "viewer@example.invalid") {
		t.Fatalf("viewer could not read own profile: %d %s", w.Code, w.Body)
	}
	identity.Role = "internal_user"
	if _, _, ok := issuer.issueSession(httptest.NewRecorder(), identity); !ok {
		t.Fatal("mapped role transition failed")
	}
	if _, authorized := keys.Authorize(token); authorized {
		t.Fatal("existing viewer token acquired inference access")
	}
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
	server.renderCLIConfirmation(page, "login", ssoIdentity{Subject: "subject", Email: "login@example.invalid", Verified: true}, server.config)
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
	handler := registry.Handler(http.NotFoundHandler())
	for _, route := range []struct {
		path   string
		status int
	}{
		{"/user/info", http.StatusOK},
		{"/user/info?user_id=other", http.StatusForbidden},
		{"/team/list", http.StatusOK},
		{"/team/info?team_id=team-a", http.StatusOK},
		{"/team/info?team_id=private", http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodGet, route.path, nil)
		request.Header.Set("Authorization", "Bearer "+result.Key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != route.status {
			t.Fatalf("%s returned %d: %s", route.path, response.Code, response.Body)
		}
	}
	registry.state.Teams["team-a"] = team{ID: "team-a", Models: []string{"approved"}}
	if _, allowed := registry.Authorize(result.Key); allowed {
		t.Fatal("the CLI key survived membership removal")
	}
}

func TestSSOProvisioningStoresUserAndDefaultTeamsAtomically(t *testing.T) {
	cfg, err := Load(strings.NewReader(`
model_list:
  - model_name: approved
    litellm_params:
      model: openai/fixture
      api_base: http://127.0.0.1:4000
litellm_settings:
  default_internal_user_params:
    user_role: internal_user
    models: [approved]
    teams:
      - provisioned
      - team_id: missing
        user_role: admin
`))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.SetSSOUserDefaults(cfg.SSOUserDefaults, cfg.SSOTeamClaim)
	registry.state.Teams["provisioned"] = team{ID: "provisioned"}
	server := &uiSSOServer{keys: registry, config: uiSSOConfig{provider: "generic", userInfo: "https://sso.example.invalid/me"}}
	first := httptest.NewRecorder()
	if _, _, ok := server.issueSession(first, ssoIdentity{Subject: "subject", Email: "LOGIN@example.invalid", Verified: true}); ok ||
		first.Code != http.StatusServiceUnavailable || len(registry.state.Users) != 0 || len(registry.state.Teams["provisioned"].Members) != 0 {
		t.Fatalf("failed provisioning committed partial state: %d %s", first.Code, first.Body)
	}
	registry.state.Teams["missing"] = team{ID: "missing"}
	if _, _, ok := server.issueSession(httptest.NewRecorder(), ssoIdentity{Subject: "subject", Email: "LOGIN@example.invalid"}); ok {
		t.Fatal("unverified email triggered account provisioning")
	}
	if _, _, ok := server.issueSession(httptest.NewRecorder(), ssoIdentity{Subject: "subject", Email: "LOGIN@example.invalid", Verified: true}); !ok {
		t.Fatal("verified SSO provisioning failed")
	}
	for _, user := range registry.state.Users {
		if user.Email != "login@example.invalid" || len(user.Models) != 1 || user.Models[0] != "approved" {
			t.Fatalf("unexpected provisioned user: %+v", user)
		}
	}
	if len(registry.state.Users) != 1 || len(registry.state.Keys) != 1 || len(registry.state.Teams["provisioned"].Members) != 1 {
		t.Fatal("missing expected user, key or default membership")
	}
	if roles := registry.state.Teams["missing"].Members; len(roles) != 1 || roles[0].Role != "admin" {
		t.Fatal("configured default team role was lost")
	}
}
