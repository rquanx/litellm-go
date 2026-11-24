package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"slices"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestMCPDiscoveryCatalogUsesUpstreamFilters(t *testing.T) {
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, token string, status int) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s returned %d, want %d: %s", path, w.Code, status, w.Body.String())
		}
		return w.Body.Bytes()
	}
	path := "/v1/mcp/discover?query=GITHUB&category=Developer+Tools"
	call(path, "wrong", http.StatusUnauthorized)
	var catalog struct {
		Servers []struct {
			Name string
		}
		Categories []string
	}
	if err := json.Unmarshal(call(path, "master", http.StatusOK), &catalog); err != nil ||
		len(catalog.Servers) != 1 || catalog.Servers[0].Name != "github" ||
		!slices.Contains(catalog.Categories, "Developer Tools") || !slices.IsSorted(catalog.Categories) {
		t.Fatalf("unexpected MCP catalog: %+v (%v)", catalog, err)
	}
	var openAPI struct {
		APIs []json.RawMessage
	}
	if err := json.Unmarshal(call("/v1/mcp/openapi-registry", "master", http.StatusOK), &openAPI); err != nil || len(openAPI.APIs) == 0 {
		t.Fatalf("invalid OpenAPI registry response: %v", err)
	}
}

func TestPublicMCPRegistryPublishesOnlySelectedServers(t *testing.T) {
	config, err := Load(strings.NewReader(`
general_settings:
  master_key: master
  enable_mcp_registry: true
mcp_servers:
  private_server:
    server_id: private-id
    alias: private-alias
    url: https://fixture.invalid/mcp
`))
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state.json")
	open := func() http.Handler {
		t.Helper()
		handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), config, "master", state)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := open()
	call := func(method, path, token, body string, want int) string {
		t.Helper()
		req := httptest.NewRequest(method, "https://gateway.example"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	const registry = "/v1/mcp/registry.json"
	const publish = "/v1/mcp/make_public"
	if body := call("GET", registry, "", "", 200); strings.Contains(body, "private") {
		t.Fatalf("unpublished server appeared in registry: %s", body)
	}
	call("POST", publish, "wrong", `{"mcp_server_ids":["private-id"]}`, 401)
	call("POST", publish, "master", `{"mcp_server_ids":["unknown"]}`, 404)
	call("POST", publish, "master", `{"mcp_server_ids":["private-id"]}`, 202)
	handler = open()
	if body := call("GET", registry, "", "", 200); !strings.Contains(body, "https://gateway.example/private-alias/mcp") || strings.Contains(body, "fixture.invalid") {
		t.Fatalf("published entry missing or contained the internal origin: %s", body)
	}
	call("POST", publish, "master", `{"mcp_server_ids":[]}`, 202)
	if body := call("GET", registry, "", "", 200); strings.Contains(body, "private") {
		t.Fatalf("unpublished server appeared in registry: %s", body)
	}
}
