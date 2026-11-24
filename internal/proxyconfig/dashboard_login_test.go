package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestProxyAdminViewerCanReadButCannotMutateOrInfer(t *testing.T) {
	t.Setenv("DISABLE_ADMIN_UI", "false")
	const master = "fixture-master"
	keys := organizationFixture(t)
	server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true, AuthorizeKey: keys.Authorize})
	models, err := WithModelManagement(server, Config{}, master, "", keys)
	if err != nil {
		t.Fatal(err)
	}
	handler := WithDashboardLogin(keys.Handler(models), master, keys)
	request := func(method, path, token, body string, status int) string {
		t.Helper()
		return organizationRequest(t, handler, method, path, token, body, status)
	}
	created := request("POST", "/user/new", master, `{"user_id":"readonly","user_email":"readonly@example.test","user_role":"proxy_admin_viewer","password":"fixture-password"}`, 200)
	var createdUser struct{ Key string }
	if err := json.Unmarshal([]byte(created), &createdUser); err != nil || createdUser.Key == "" {
		t.Fatalf("viewer did not receive a credential: %s", created)
	}
	login := request("POST", "/v2/login", "", `{"username":"readonly@example.test","password":"fixture-password"}`, 200)
	var session struct{ Token string }
	if err := json.Unmarshal([]byte(login), &session); err != nil || session.Token == "" {
		t.Fatalf("viewer login failed: %s", login)
	}
	parsed, err := jwt.Parse(session.Token, func(*jwt.Token) (any, error) { return []byte(master), nil },
		jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("invalid dashboard session: %v", err)
	}
	sessionKey, _ := parsed.Claims.(jwt.MapClaims)["key"].(string)
	if sessionKey == "" {
		t.Fatal("dashboard session omitted its scoped key")
	}
	request("POST", "/credentials", master, `{"credential_name":"provider","credential_values":{"api_key":"fixture-provider-secret"}}`, 200)
	for _, token := range []string{createdUser.Key, sessionKey} {
		request("GET", "/user/list", token, "", 200)
		request("GET", "/organization/info?organization_id=delegation-other", token, "", 200)
		request("GET", "/v1/access_group", token, "", 200)
		request("GET", "/router/settings", token, "", 200)
		credential := request("GET", "/credentials/by_name/provider", token, "", 200)
		if !strings.Contains(credential, "********") || strings.Contains(credential, "fixture-provider-secret") {
			t.Fatalf("credential details were not masked: %s", credential)
		}
		request("PATCH", "/credentials/provider", token, `{"credential_values":{"api_key":"new-value"}}`, 401)
		request("GET", "/v1/models", token, "", 401)
		request("POST", "/v1/access_group", token, `{"access_group_name":"test"}`, 401)
		request("PATCH", "/organization/update", token, `{"organization_id":"delegation-other","organization_alias":"changed"}`, 401)
		request("POST", "/key/generate", token, `{}`, 401)
	}
}

func TestInternalUserDashboardPasswordAndSessionRevocation(t *testing.T) {
	t.Setenv("UI_USERNAME", "operator")
	t.Setenv("UI_PASSWORD", "fixture-operator-password")
	t.Setenv("DISABLE_ADMIN_UI", "false")
	const master = "fixture-master"
	state := filepath.Join(t.TempDir(), "keys.json")
	boot := func() (http.Handler, *KeyRegistry) {
		t.Helper()
		keys, err := LoadKeys(master, state)
		if err != nil {
			t.Fatal(err)
		}
		return WithDashboardLogin(keys.Handler(http.NotFoundHandler()), master, keys), keys
	}
	handler, keys := boot()
	call := func(method, path, token, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s returned %d, want %d", method, path, response.Code, status)
		}
		return response.Body.String()
	}
	created := call("POST", "/user/new", master, `{"user_id":"viewer","user_email":"fixture@example.test","user_role":"internal_user_viewer","password":"fixture-password","auto_create_key":false}`, 200)
	if strings.Contains(created, "fixture-password") || strings.Contains(created, "scrypt:") {
		t.Fatal("password material appeared in the user response")
	}
	handler, keys = boot()
	if !strings.HasPrefix(keys.state.Passwords["viewer"], "scrypt:") {
		t.Fatal("password hash was not persisted")
	}
	login := func(password string, status int) string {
		return call("POST", "/v2/login", "", `{"username":"FIXTURE@example.test","password":"`+password+`"}`, status)
	}
	login("wrong", http.StatusUnauthorized)
	var result struct{ Token string }
	if err := json.Unmarshal([]byte(login("fixture-password", 200)), &result); err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Parse(result.Token, func(*jwt.Token) (any, error) { return []byte(master), nil })
	if err != nil {
		t.Fatal(err)
	}
	claims := token.Claims.(jwt.MapClaims)
	key, _ := claims["key"].(string)
	if key == "" || claims["user_role"] != "internal_user_viewer" || claims["user_id"] != "viewer" {
		t.Fatal("login did not produce a scoped viewer credential")
	}
	if _, allowed := keys.Authorize(key); allowed {
		t.Fatal("viewer key can call inference")
	}
	profile := call("GET", "/v2/user/info?user_id=viewer", key, "", 200)
	if strings.Contains(profile, "scrypt:") {
		t.Fatal("profile exposed the password hash")
	}
	call("POST", "/user/update", master, `{"user_id":"viewer","password":"new-fixture-password"}`, 200)
	call("GET", "/v2/user/info?user_id=viewer", key, "", 401)
	login("fixture-password", 401)
	login("new-fixture-password", 200)
}

func TestDashboardInvitationClaimIsBoundAndSingleUse(t *testing.T) {
	t.Setenv("DISABLE_ADMIN_UI", "false")
	const master = "fixture-master"
	state := filepath.Join(t.TempDir(), "keys.json")
	boot := func() http.Handler {
		t.Helper()
		keys, err := LoadKeys(master, state)
		if err != nil {
			t.Fatal(err)
		}
		return WithDashboardLogin(keys.Handler(http.NotFoundHandler()), master, keys)
	}
	handler := boot()
	call := func(method, path, key, body string, status int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, status, w.Body)
		}
		return w.Body.Bytes()
	}
	call("POST", "/user/new", master, `{"user_id":"fixture","user_email":"invite@example.test","auto_create_key":false}`, 200)
	var invite dashboardInvitation
	if err := json.Unmarshal(call("POST", "/invitation/new", master, `{"user_id":"fixture"}`, 200), &invite); err != nil || invite.ID == "" {
		t.Fatalf("missing invitation: %v", err)
	}
	handler = boot()
	lookup := "/onboarding/get_token?invite_link=" + invite.ID
	var result struct{ Token string }
	if err := json.Unmarshal(call("GET", lookup, "", "", 200), &result); err != nil {
		t.Fatal(err)
	}
	wrapper, err := jwt.Parse(result.Token, func(*jwt.Token) (any, error) { return []byte(master), nil })
	if err != nil {
		t.Fatal(err)
	}
	key := wrapper.Claims.(jwt.MapClaims)["key"].(string)
	claim := "/onboarding/claim_token"
	body := `{"invitation_link":"`+invite.ID+`","user_id":"fixture","password":"fixture-invite-password"}`
	call("POST", claim, "", body, 401)
	call("POST", claim, key, strings.Replace(body, `"fixture"`, `"other"`, 1), 401)
	call("GET", "/v2/user/info?user_id=fixture", key, "", 401)
	response := call("POST", claim, key, body, 200)
	if strings.Contains(string(response), "fixture-invite-password") || strings.Contains(string(response), "scrypt:") {
		t.Fatal("onboarding response exposed password material")
	}
	handler = boot()
	call("POST", claim, key, body, 401)
	call("GET", lookup, "", "", 401)
	call("POST", "/v2/login", "", `{"username":"invite@example.test","password":"fixture-invite-password"}`, 200)
}

func TestDashboardPasswordLoginIssuesExpiringCredential(t *testing.T) {
	t.Setenv("UI_USERNAME", "operator")
	t.Setenv("UI_PASSWORD", "dashboard-fixture")
	t.Setenv("DISABLE_ADMIN_UI", "false")
	t.Setenv("PROXY_BASE_URL", "https://proxy.invalid/gateway")
	const master = "fixture-master"
	registry, err := LoadKeys(master, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := WithDashboardLogin(registry.Handler(http.NotFoundHandler()), master)
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, r)
		return result
	}
	if form := request("GET", "/sso/key/generate", "", ""); form.Code != 200 || !strings.Contains(form.Body.String(), "<form") {
		t.Fatalf("dashboard password login was unavailable: %d", form.Code)
	}
	if invalid := request("POST", "/v2/login", "", `{"username":"operator","password":"wrong"}`); invalid.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credentials returned %d", invalid.Code)
	}
	response := request("POST", "/v2/login", "", `{"username":"operator","password":"dashboard-fixture"}`)
	var result struct {
		Token string
		URL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 ||
		result.URL != "https://proxy.invalid/gateway/ui/?login=success" {
		t.Fatalf("unexpected response %d %s: %v", response.Code, response.Body, err)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || cookies[0].Name != "token" || cookies[0].Value != result.Token {
		t.Fatalf("unexpected browser cookies %v", cookies)
	}
	signed, err := jwt.Parse(result.Token, func(*jwt.Token) (any, error) { return []byte(master), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := signed.Claims.(jwt.MapClaims)["key"].(string)
	if key == master || !strings.HasPrefix(key, "litellm-ui.") {
		t.Fatal("browser received the master key or lacked a dashboard credential")
	}
	if saved := request("POST", "/key/generate", key, `{}`); saved.Code != http.StatusOK {
		t.Fatalf("dashboard key was rejected: %d %s", saved.Code, saved.Body)
	}
	if info := request("GET", "/v2/user/info?user_id=default_user_id", key, ""); info.Code != 200 || !strings.Contains(info.Body.String(), "proxy_admin") {
		t.Fatalf("dashboard admin profile did not load: %d %s", info.Code, info.Body)
	}
	if rejected := request("POST", "/key/generate", "litellm-ui."+result.Token, `{}`); rejected.Code != http.StatusUnauthorized {
		t.Fatalf("a cookie was accepted as an admin bearer key: %d", rejected.Code)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if runtime.ProxyCredential(r) != "" {
		t.Fatal("unexpected proxy credential")
	}
}
