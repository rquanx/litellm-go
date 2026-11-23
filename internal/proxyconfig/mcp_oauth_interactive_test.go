package proxyconfig

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestInteractiveMCPOAuthUsesBoundPKCEGrant(t *testing.T) {
	var exchanges atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if r.FormValue("redirect_uri") != "http://127.0.0.1:4300/callback" || r.FormValue("client_secret") != "fixture-secret" ||
			r.FormValue("grant_type") != "authorization_code" || r.FormValue("code") != "fixture-code" {
			t.Error("incorrect upstream grant form")
		}
		io.WriteString(w, `{"access_token":"fixture-access","token_type":"Bearer"}`)
	}))
	defer provider.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
mcp_servers:
  fixture:
    url: %q
    auth_type: oauth2
    oauth2_flow: authorization_code
    authorization_url: %q
    token_url: %q
    oauth_callback_url: http://127.0.0.1:4300/callback
    client_id: fixture-client
    client_secret: fixture-secret
    scopes: [read]
    allow_all_keys: true
`, provider.URL+"/mcp", provider.URL+"/authorize", provider.URL+"/token")))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master", MCPServers: config.MCPServers,
		AuthorizeKey: func(key string) ([]string, bool) { return nil, key == "other" }})
	call := func(method, path, token, form string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(form))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if form != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		return result
	}
	verifier := strings.Repeat("v", 43)
	digest := sha256.Sum256([]byte(verifier))
	values := url.Values{"redirect_uri": {"http://127.0.0.1:5321/finish?session=demo"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"}, "state": {"client-state"}}
	result := call("GET", "/fixture/authorize?"+values.Encode(), "master", "")
	redirect, err := url.Parse(result.Header().Get("Location"))
	if result.Code != http.StatusFound || err != nil || redirect.Query().Get("client_id") != "fixture-client" ||
		redirect.Query().Get("scope") != "read" || redirect.Query().Get("state") == "" {
		t.Fatalf("incorrect OAuth redirect %d %s", result.Code, result.Body)
	}
	state := redirect.Query().Get("state")
	result = call("GET", "/callback?code=fixture-code&state="+url.QueryEscape(state), "", "")
	redirect, err = url.Parse(result.Header().Get("Location"))
	if result.Code != http.StatusFound || err != nil || redirect.Query().Get("state") != "client-state" ||
		redirect.Query().Get("session") != "demo" || redirect.Query().Get("code") != "fixture-code" {
		t.Fatalf("incorrect OAuth callback %d %s", result.Code, result.Body)
	}
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "code": {"fixture-code"}, "code_verifier": {verifier}}
	for _, key := range []string{"other", "master", "master"} {
		result := call("POST", "/fixture/token", key, tokenForm.Encode())
		if key == "master" && exchanges.Load() == 1 && result.Code == http.StatusOK {
			if !strings.Contains(result.Body.String(), "fixture-access") || result.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("incorrect token response %s", result.Body)
			}
		} else if result.Code != http.StatusBadRequest {
			t.Fatalf("invalid token request accepted %d %s", result.Code, result.Body)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("expected one token exchange, got %d", exchanges.Load())
	}
}
