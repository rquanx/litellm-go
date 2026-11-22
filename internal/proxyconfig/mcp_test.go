package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

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
    allow_all_keys: true
    allowed_tools: [echo]
    headers: {Authorization: Bearer mcp-fixture}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{AuthorizeInference: true, Token: "fixture-master", MCPServers: cfg.MCPServers,
		AuthorizeKey: func(token string) ([]string, bool) { return nil, token == "fixture-key" }})
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
	call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"private"}`, 403)
	if result := call("POST", "/mcp-rest/tools/call", "fixture-key", `{"server_id":"fixture","name":"echo"}`, 200); !strings.Contains(result, "fixture") {
		t.Fatalf("unexpected tool response %s", result)
	}
	if closed.Load() != 2 {
		t.Fatalf("closed %d MCP sessions", closed.Load())
	}
}
