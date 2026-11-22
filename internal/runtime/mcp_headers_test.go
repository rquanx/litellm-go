package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPHeadersUseMostSpecificServerPrefix(t *testing.T) {
	first := MCPServer{ID: "first", Name: "demo"}
	second := MCPServer{ID: "second", Name: "demo-extra", Headers: http.Header{"X-Api-Key": []string{"configured"}}}
	server := NewServer(Config{MCPServers: []MCPServer{first, second}})
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("X-MCP-Auth", "Bearer generic")
	request.Header.Set("X-MCP-Demo-Extra-Authorization", "Bearer scoped")
	request.Header.Set("X-MCP-Demo-Extra-X-Api-Key", "override")
	request.Header.Set("X-MCP-Servers", "demo-extra")
	if got := server.mcpRequestServer(request, first).Headers.Get("Authorization"); got != "Bearer generic" {
		t.Fatalf("unexpected first-server authorization %q", got)
	}
	headers := server.mcpRequestServer(request, second).Headers
	if headers.Get("Authorization") != "Bearer scoped" || headers.Get("X-Api-Key") != "configured" {
		t.Fatal("MCP authorization precedence changed")
	}
	access := filterMCPAccess(request, func(MCPServer, string) bool { return true })
	if access(first, "") || !access(second, "") {
		t.Fatal("server selector did not narrow access")
	}
}
