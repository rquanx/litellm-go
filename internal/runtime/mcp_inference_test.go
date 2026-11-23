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
		case "tools/call":
			if !strings.Contains(string(req.Params), `"name":"allowed"`) {
				t.Errorf("wrong MCP tool %s", req.Params)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"tool-result"}]}}`, req.ID)
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
		if strings.Contains(string(raw), "function_call_output") {
			if !strings.Contains(string(raw), "tool-result") {
				t.Errorf("missing MCP result %s", raw)
			}
			if strings.Contains(string(raw), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"follow-up-stream\",\"status\":\"completed\",\"output\":[]}}\n\n")
				return
			}
			io.WriteString(w, `{"id":"follow-up","object":"response","output":[]}`)
			return
		}
		io.WriteString(w, `{"id":"fixture-response","object":"response","output":[{"type":"function_call","name":"fixture-allowed","call_id":"call1","arguments":"{}"}]}`)
	}))
	defer model.Close()
	server := NewServer(Config{AuthorizeInference: true, Token: "master", MaxConcurrent: 1,
		MCPServers: []MCPServer{{ID: "fixture", Name: "fixture", URL: mcp.URL, AllowedTools: []string{"allowed"}}},
		Routes:     []Route{{Alias: "demo", Candidates: []Candidate{{Provider: "openai", Endpoint: "responses", ModelID: "demo", APIBase: model.URL}}}},
	})
	server.ReplaceMCPToolsets([]MCPToolset{{ID: "selected", Name: "picked", Tools: []MCPToolsetEntry{{ServerID: "fixture", Name: "allowed"}}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"fixture","tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/fixture","allowed_tools":["allowed","private"]}]}`))
	request.Header.Set("Authorization", "Bearer master")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "fixture-response") {
		t.Fatalf("unexpected inference result %d %s", result.Code, result.Body)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"fixture","tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/picked","require_approval":"never"}]}`))
	request.Header.Set("Authorization", "Bearer master")
	result = httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "follow-up") {
		t.Fatalf("unexpected automatic MCP result %d %s", result.Code, result.Body)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"demo","input":"fixture","stream":true,"tools":[{"type":"mcp","server_url":"litellm_proxy/mcp/picked","require_approval":"never"}]}`))
	request.Header.Set("Authorization", "Bearer master")
	result = httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "follow-up-stream") ||
		!strings.Contains(result.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected streaming MCP result %d %s", result.Code, result.Body)
	}
}
