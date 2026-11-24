package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestMCPClientCredentialsReuseAndRefresh(t *testing.T) {
	var tokens atomic.Int32
	var reject atomic.Bool
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" ||
				r.Form.Get("client_id") != "fixture-client" || r.Form.Get("client_secret") != "fixture-secret" ||
				r.Form.Get("scope") != "mcp:tools" {
				t.Error("unexpected OAuth grant payload")
			}
			fmt.Fprintf(w, `{"access_token":"fixture-token-%d","token_type":"Bearer","expires_in":3600}`, tokens.Add(1))
			return
		}
		if got := r.Header.Get("Authorization"); got != fmt.Sprintf("Bearer fixture-token-%d", tokens.Load()) {
			t.Errorf("incorrect provider token")
		}
		if reject.Swap(false) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			// Empty initialize replies are enough to exercise credentials and cache behavior.
			fmt.Fprint(w, `{}`)
		}
	}))
	defer fixture.Close()
	t.Setenv("MINATO_TEST_MCP_BASE", fixture.URL)
	t.Setenv("MINATO_TEST_MCP_SECRET", "fixture-secret")
	config, err := Load(strings.NewReader(`
mcp_servers:
  fixture:
    url: os.environ/MINATO_TEST_MCP_BASE
    auth_type: oauth2
    token_url: ` + fixture.URL + `/token
    client_id: fixture-client
    client_secret: os.environ/MINATO_TEST_MCP_SECRET
    scopes: [mcp:tools]
general_settings: {master_key: master}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "master", MCPServers: config.MCPServers})
	call := func() {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.Header.Set("Authorization", "Bearer master")
		r.Header.Set("X-MCP-Auth", "Bearer arbitrary")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected MCP status %d: %s", w.Code, w.Body)
		}
	}
	call()
	call()
	if tokens.Load() != 1 {
		t.Fatalf("expected one OAuth exchange, got %d", tokens.Load())
	}
	reject.Store(true)
	call()
	call()
	if tokens.Load() != 2 {
		t.Fatalf("expected token refresh, got %d exchanges", tokens.Load())
	}
	if entry := mcpServerEntry("fixture", config.staticMCPServers["fixture"]); entry.ClientSecret != "" {
		t.Fatal("OAuth client secret is exposed in management output")
	}
}
