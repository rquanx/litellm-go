package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPStreamableSessionReusesAndClosesUpstream(t *testing.T) {
	var opened, closed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "fixture-upstream" {
				t.Error("wrong upstream session")
			}
			closed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req mcpRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		switch req.Method {
		case "initialize":
			opened.Add(1)
			w.Header().Set("Mcp-Session-Id", "fixture-upstream")
			writeMCPResult(w, req.ID, map[string]string{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "fixture-upstream" {
				t.Error("wrong upstream session")
			}
			writeMCPResult(w, req.ID, map[string]any{"tools": []map[string]any{{"name": "read"}}})
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "fixture-upstream" {
				t.Error("wrong upstream session")
			}
			writeMCPResult(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "fixture"}}})
		default:
			t.Errorf("unexpected MCP method %s", req.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master",
		AuthorizeMCP: func(key string) (MCPAccess, bool) {
			return func(MCPServer, string) bool { return true }, key == "other"
		},
		MCPServers: []MCPServer{{ID: "test", Name: "test", Transport: "http", URL: upstream.URL}},
	})
	call := func(method, path, body, key, id, protocol string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Mcp-Session-Id", id)
		r.Header.Set("MCP-Protocol-Version", protocol)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d: %s", method, path, w.Code, w.Body)
		}
		return w
	}
	init := call("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "master", "", "", 200)
	id := init.Header().Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("missing streamable session ID")
	}
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	call("POST", "/mcp", list, "other", id, "2025-06-18", 404)
	call("POST", "/test/mcp", list, "master", id, "2025-06-18", 404)
	call("POST", "/mcp", list, "master", id, "2025-03-26", 400)
	response := call("POST", "/mcp", list, "master", id, "2025-06-18", 200)
	if !strings.Contains(response.Body.String(), "test-read") {
		t.Fatal(response.Body)
	}
	tool := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q}}`, "test-read")
	call("POST", "/mcp", tool, "master", id, "2025-06-18", 200)
	call("DELETE", "/mcp", "", "master", id, "2025-06-18", 204)
	call("POST", "/mcp", list, "master", id, "2025-06-18", 404)
	if opened.Load() != 1 || closed.Load() != 1 {
		t.Fatalf("upstream opened %d and closed %d times", opened.Load(), closed.Load())
	}
}
