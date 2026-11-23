package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testMCPUserTokens struct {
	mu    sync.Mutex
	token MCPUserOAuthToken
}

func (store *testMCPUserTokens) Read(context.Context, string, string) (MCPUserOAuthToken, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.token, nil
}

func (store *testMCPUserTokens) Save(_ context.Context, _, _ string, token MCPUserOAuthToken) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.token = token
	return nil
}

func TestMCPRefreshesExpiredUserTokenOnce(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		if r.FormValue("refresh_token") != "fixture-refresh" || r.FormValue("grant_type") != "refresh_token" {
			t.Error("incorrect MCP refresh")
		}
		io.WriteString(w, `{"access_token":"updated","expires_in":3600}`)
	}))
	defer upstream.Close()
	tokens := &testMCPUserTokens{token: MCPUserOAuthToken{AccessToken: "expired", RefreshToken: "fixture-refresh", ExpiresAt: time.Now().Add(-time.Minute)}}
	server := NewServer(Config{MCPUserOAuthStore: tokens, MCPUserOAuthIdentity: func(*http.Request) string { return "fixture-user" }})
	mcp := MCPServer{ID: "fixture-server", InteractiveOAuth: &MCPInteractiveOAuth{TokenURL: upstream.URL, ClientID: "fixture"}}
	var requests sync.WaitGroup
	for range 4 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if updated := server.loadMCPUserOAuth(r, mcp); updated != "updated" {
				t.Error("missing refreshed token")
			}
		}()
	}
	requests.Wait()
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 1 {
		t.Fatalf("expected one refresh, got %d", calls)
	}
}

func (store *testMCPUserTokens) Delete(context.Context, string, string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.token = MCPUserOAuthToken{}
	return nil
}

func (store *testMCPUserTokens) List(context.Context, string) (map[string]MCPUserOAuthToken, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return map[string]MCPUserOAuthToken{"fixture-server": store.token}, nil
}

func TestMCPUserCredentialManagementKeepsTokensPrivate(t *testing.T) {
	tokens := &testMCPUserTokens{}
	server := NewServer(Config{
		AuthorizeInference: true, Token: "master", MCPUserOAuthStore: tokens,
		AuthorizeKey: func(key string) ([]string, bool) { return nil, key == "user" },
		MCPUserOAuthIdentity: func(r *http.Request) string {
			if ProxyCredential(r) == "user" {
				return "owner"
			}
			return ""
		},
		MCPServers: []MCPServer{{ID: "fixture-server", Name: "fixture", AllowAllKeys: true, InteractiveOAuth: &MCPInteractiveOAuth{}}},
	})
	const path = "/mcp/server/fixture-server/oauth-user-credential"
	call := func(method, path, body, want string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer user")
		result := httptest.NewRecorder()
		server.ServeHTTP(result, r)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), want) ||
			strings.Contains(result.Body.String(), "private-token") || strings.Contains(result.Body.String(), "private-refresh") {
			t.Fatalf("wrong management result %d %s", result.Code, result.Body)
		}
	}
	call("POST", path, `{"access_token":"private-token","refresh_token":"private-refresh","expires_in":3600}`, `"has_credential":true`)
	call("GET", path+"/status", "", `"has_credential":true`)
	call("GET", "/mcp/user-credentials", "", `"server_name":"fixture"`)
	call("DELETE", path, "", `"has_credential":false`)
	call("GET", path+"/status", "", `"has_credential":false`)
}
