package runtime

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMCPStdioTransport(t *testing.T) {
	if os.Getenv("MINATO_TEST_MCP_CHILD") == "1" {
		decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
		encoder := json.NewEncoder(os.Stdout)
		var request mcpRPCRequest
		for decoder.Decode(&request) == nil {
			var result any
			switch request.Method {
			case "initialize":
				result = map[string]string{"protocolVersion": "2025-03-26"}
			case "tools/list":
				result = map[string]any{"tools": []map[string]any{{"name": "stdio-fixture", "inputSchema": map[string]string{"type": "object"}}}}
			default:
				continue
			}
			if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(Config{AuthorizeInference: true, MCPServers: []MCPServer{{Name: "local", ID: "local", Command: binary,
		Args: []string{"-test.run=^TestMCPStdioTransport$"}, Env: []string{"MINATO_TEST_MCP_CHILD=1"}}}})
	output := httptest.NewRecorder()
	server.ServeHTTP(output, httptest.NewRequest(http.MethodGet, "/mcp-rest/tools/list?server_id=local", nil))
	if output.Code != 200 || !strings.Contains(output.Body.String(), "stdio-fixture") {
		t.Fatalf("unexpected stdio tool list: %d %s", output.Code, output.Body.String())
	}
}
