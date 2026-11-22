package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestManagedMCPServersPersistAndPublish(t *testing.T) {
	const master = "fixture-master"
	state := filepath.Join(t.TempDir(), "models.json")
	open := func() (http.Handler, *runtime.Server) {
		server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true})
		handler, err := WithModelManagement(server, Config{}, master, state)
		if err != nil {
			t.Fatal(err)
		}
		return handler, server
	}
	handler, _ := open()
	call := func(method, path, payload string, expected int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+master)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != expected || strings.Contains(result.Body.String(), "fixture-provider-secret") {
			t.Fatalf("%s %s got %d, want %d: %s", method, path, result.Code, expected, result.Body.String())
		}
		return result.Body.Bytes()
	}
	var created mcpServerInput
	raw := call("POST", mcpServerPath, `{"server_name":"fixture","transport":"http","url":"http://localhost:1/mcp","static_headers":{"Authorization":"Bearer fixture-provider-secret"}}`, 200)
	if json.Unmarshal(raw, &created) != nil || created.ServerID == "" {
		t.Fatalf("missing managed MCP server ID in %s", raw)
	}
	handler, _ = open()
	call("GET", mcpServerPath+"/"+created.ServerID, "", 200)
	call("PUT", mcpServerPath, `{"server_id":"`+created.ServerID+`","server_name":"renamed","transport":"http","url":"http://localhost:1/mcp"}`, 200)
	call("DELETE", mcpServerPath+"/"+created.ServerID, "", 200)
	handler, _ = open()
	call("GET", mcpServerPath+"/"+created.ServerID, "", 404)
}

func TestMCPRESTUsesScopedStreamableHTTPSessions(t *testing.T) {
	var closed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mcp-fixture" {
			t.Error("MCP server authorization was missing")
		}
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "test-session" {
				t.Error("MCP session ID missing on termination")
			}
			closed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var message struct {
			ID string `json:"id"`
			Method string `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&message) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if message.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "test-session" {
			t.Error("MCP session ID was not propagated")
		}
		var result json.RawMessage
		switch message.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "test-session")
			result = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{"tools":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"echo","inputSchema":{"type":"object"}},{"name":"private","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			if message.Params["name"] != "echo" {
				t.Error("unexpected MCP tool call")
			}
			result = json.RawMessage(`{"content":[{"type":"text","text":"fixture"}],"isError":false}`)
		case "prompts/list":
			result = json.RawMessage(`{"prompts":[{"name":"sample","description":"fixture"}]}`)
		case "prompts/get":
			if message.Params["name"] != "sample" {
				t.Error("unexpected MCP prompt name")
			}
			result = json.RawMessage(`{"messages":[{"role":"user","content":{"type":"text","text":"fixture prompt"}}]}`)
		case "resources/list":
			result = json.RawMessage(`{"resources":[{"name":"manual","uri":"file:///fixture"}]}`)
		case "resources/read":
			if message.Params["uri"] != "file:///fixture" {
				t.Error("unexpected MCP resource URI")
			}
			result = json.RawMessage(`{"contents":[{"uri":"file:///fixture","text":"fixture resource"}]}`)
		default:
			t.Error("unexpected MCP request")
		}
		event, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message\ndata: " + string(event) + "\n\n"))
	}))
	defer upstream.Close()
	t.Setenv("TEST_MCP_URL", upstream.URL)
	cfg, err := Load(strings.NewReader(`
mcp_servers:
  fixture:
    url: os.environ/TEST_MCP_URL
    transport: http
    mcp_access_groups: [fixture-group]
    allowed_tools: [echo]
    headers: {Authorization: Bearer mcp-fixture}
`))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := LoadKeys("fixture-master", "")
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Teams["fixture-team"] = team{ID: "fixture-team", ObjectPermission: &objectPermission{
		MCPAccessGroups: []string{"fixture-group"}, MCPTools: map[string][]string{"fixture": {"echo"}},
	}}
	registry.state.Keys[keyDigest("fixture-key")] = virtualKey{TeamID: "fixture-team"}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "fixture-master", MCPServers: cfg.MCPServers,
		AuthorizeMCP: registry.AuthorizeMCP})
	call := func(method, p, key, body string, status int) string {
		t.Helper()
		request := httptest.NewRequest(method, p, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		output := httptest.NewRecorder()
		server.ServeHTTP(output, request)
		if output.Code != status {
			t.Fatalf("%s returned %d: %s", p, output.Code, output.Body.String())
		}
		return output.Body.String()
	}
	if result := call("GET", "/mcp-rest/tools/list", "fixture-key", "", 200); !strings.Contains(result, "echo") || strings.Contains(result, "private") {
		t.Fatalf("unexpected tools %s", result)
	}
	if result := call("GET", "/v1/mcp/access_groups", "fixture-key", "", 200); !strings.Contains(result, "fixture-group") {
		t.Fatalf("unexpected MCP access groups %s", result)
	}
	call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"private"}`, 403)
	if result := call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"echo"}`, 200); !strings.Contains(result, "fixture") {
		t.Fatalf("unexpected tool response %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, 200); !strings.Contains(result, "protocolVersion") {
		t.Fatalf("unexpected MCP initialization response %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, 200); !strings.Contains(result, "fixture-echo") {
		t.Fatalf("unexpected MCP tool list %s", result)
	}
	if result := call("POST", "/mcp", "fixture-key", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fixture-echo"}}`, 200); !strings.Contains(result, "fixture") {
		t.Fatalf("unexpected MCP tool response %s", result)
	}
	for _, tc := range []struct {
		body, expected string
	}{
		{`{"jsonrpc":"2.0","id":4,"method":"prompts/list"}`, "fixture-sample"},
		{`{"jsonrpc":"2.0","id":5,"method":"prompts/get","params":{"name":"fixture-sample"}}`, "fixture prompt"},
		{`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`, "fixture-manual"},
		{`{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///fixture"}}`, "fixture resource"},
	} {
		if result := call("POST", "/mcp", "fixture-key", tc.body, 200); !strings.Contains(result, tc.expected) {
			t.Fatalf("unexpected MCP response %s", result)
		}
	}
	if closed.Load() != 8 {
		t.Fatalf("closed %d MCP sessions", closed.Load())
	}
}
