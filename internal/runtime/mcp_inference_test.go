package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInferenceExpandsOnlyPermittedMCPTools(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26"}}`, req.ID)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"allowed","description":"fixture"},{"name":"private"}]}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected MCP operation %s", req.Method)
		}
	}))
	defer mcp.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"name":"fixture-allowed"`) || !strings.Contains(string(raw), `"additionalProperties":false`) ||
			strings.Contains(string(raw), "fixture-private") || strings.Contains(string(raw), "litellm_proxy/mcp") {
			t.Errorf("invalid model tools %s", raw)
		}
		io.WriteString(w, `{"id":"fixture-response","object":"response","output":[]}`)
	}))
	defer model.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master", MaxConcurrent: 1,
		MCPServers: []MCPServer{{ID: "fixture", Name: "fixture", URL: mcp.URL, AllowedTools: []string{"allowed"}}},
		Routes:     []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "openai", Endpoint: "responses", ModelID: "demo", APIBase: model.URL}}}},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"fixture","tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/fixture","allowed_tools":["allowed","private"]}]}`))
	request.Header.Set("Authorization", "Bearer master")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("unexpected inference result %d %s", result.Code, result.Body)
	}
}
