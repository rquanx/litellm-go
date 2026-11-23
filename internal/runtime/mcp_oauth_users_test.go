package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
