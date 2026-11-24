package runtime

import "testing"

func TestGlobalToolPolicyBlocksMCPTool(t *testing.T) {
	server := NewServer(Config{})
	server.ReplaceToolPolicies(map[string]ToolPolicy{"gateway-search": {Input: "blocked"}})
	if server.toolPolicyAllowed(MCPServer{Name: "gateway"}, "search") {
		t.Fatal("global blocked policy did not apply to MCP tool")
	}
	if !server.toolPolicyAllowed(MCPServer{Name: "gateway"}, "other") {
		t.Fatal("unrelated MCP tool was blocked")
	}
}

func TestTrustedToolRejectsPriorToolOutput(t *testing.T) {
	fields := map[string]any{"messages": []any{map[string]any{"role": "tool", "content": "untrusted"}}}
	if !mcpInferenceHasToolOutput(fields) { t.Fatal("tool output was not detected") }
	if mcpInferenceHasToolOutput(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}) { t.Fatal("ordinary message was treated as tool output") }
}

func TestSemanticFilterRanksMCPTools(t *testing.T) {
	server := NewServer(Config{})
	server.ReplaceMCPSemanticFilter(MCPSemanticFilterSettings{Enabled: true, TopK: 1, SimilarityThreshold: 0.1})
	entries := []map[string]any{{"name": "weather", "description": "current weather forecast"}, {"name": "database", "description": "query SQL records"}}
	filtered := server.semanticFilterMCPTools(entries, "weather forecast")
	if len(filtered) != 1 || filtered[0]["name"] != "weather" { t.Fatalf("unexpected semantic filter result: %+v", filtered) }
}
