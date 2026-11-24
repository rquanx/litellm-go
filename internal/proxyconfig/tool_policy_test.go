package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestToolPolicyOptionsMatchesLiteLLMShape(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "", keys)
	if err != nil { t.Fatal(err) }
	req := httptest.NewRequest(http.MethodGet, "/v1/tool/policy/options", nil)
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder(); handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"value":"blocked"`) || !strings.Contains(w.Body.String(), `"output_policies"`) {
		t.Fatalf("unexpected tool policy options %d: %s", w.Code, w.Body.String())
	}
}

func TestToolPolicyLifecycleInMemory(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "", keys)
	if err != nil { t.Fatal(err) }
	call := func(method, path, body string, want int) string {
		req := httptest.NewRequest(method, path, strings.NewReader(body)); req.Header.Set("Authorization", "Bearer master")
		w := httptest.NewRecorder(); handler.ServeHTTP(w, req); if w.Code != want { t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body.String()) }; return w.Body.String()
	}
	call(http.MethodPost, "/v1/tool/policy", `{"tool_name":"search","input_policy":"trusted","output_policy":"untrusted"}`, http.StatusOK)
	if body := call(http.MethodGet, "/v1/tool/list", "", http.StatusOK); !strings.Contains(body, `"tool_name":"search"`) { t.Fatalf("tool missing from list: %s", body) }
	if body := call(http.MethodGet, "/v1/tool/search", "", http.StatusOK); !strings.Contains(body, `"input_policy":"trusted"`) { t.Fatalf("tool detail missing policy: %s", body) }
}

func TestToolPolicyScopedOverrideAffectsMCPAuthorization(t *testing.T) {
	keys, err := LoadKeys("master", "")
	if err != nil { t.Fatal(err) }
	keys.state.Teams["team"] = team{ID: "team"}
	keys.state.Keys[keyDigest("scoped-key")] = virtualKey{TeamID: "team"}
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), Config{}, "master", "", keys)
	if err != nil { t.Fatal(err) }
	req := httptest.NewRequest(http.MethodPost, "/v1/tool/policy", strings.NewReader(`{"tool_name":"dangerous","input_policy":"blocked","team_id":"team"}`))
	req.Header.Set("Authorization", "Bearer master")
	w := httptest.NewRecorder(); handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK { t.Fatalf("scoped policy update returned %d: %s", w.Code, w.Body.String()) }
	access, ok := keys.AuthorizeMCP("scoped-key")
	if !ok || access(runtime.MCPServer{ID: "server"}, "dangerous") { t.Fatal("team tool policy did not block MCP tool") }
}
