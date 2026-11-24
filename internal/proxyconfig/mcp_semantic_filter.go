package proxyconfig

import (
	"net/http"
)

func (registry *modelRegistry) registerMCPSemanticFilterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /get/mcp_semantic_filter_settings", registry.getMCPSemanticFilterSettings)
	mux.HandleFunc("PATCH /update/mcp_semantic_filter_settings", registry.updateMCPSemanticFilterSettings)
}

func (registry *modelRegistry) getMCPSemanticFilterSettings(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) { return }
	registry.mu.Lock(); values := cloneAnyMap(registry.mcpSemanticFilter); registry.mu.Unlock()
	if values == nil { values = map[string]any{"enabled": false, "embedding_model": "text-embedding-3-small", "top_k": 10, "similarity_threshold": 0.3} }
	modelJSON(w, http.StatusOK, map[string]any{"values": values, "field_schema": map[string]any{"properties": map[string]any{"enabled": map[string]string{"type":"boolean"}, "embedding_model": map[string]string{"type":"string"}, "top_k": map[string]string{"type":"integer"}, "similarity_threshold": map[string]string{"type":"number"}}}})
}

func (registry *modelRegistry) updateMCPSemanticFilterSettings(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) { return }
	var input struct { Enabled *bool `json:"enabled"`; EmbeddingModel string `json:"embedding_model"`; TopK int `json:"top_k"`; Threshold float64 `json:"similarity_threshold"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &input) != nil || input.TopK < 1 || input.TopK > 100 || input.Threshold < 0 || input.Threshold > 1 { modelError(w, http.StatusBadRequest, "Invalid MCP semantic filter settings"); return }
	enabled := false; if input.Enabled != nil { enabled = *input.Enabled }
	if input.EmbeddingModel == "" { input.EmbeddingModel = "text-embedding-3-small" }
	registry.mu.Lock()
	next := map[string]any{"enabled": enabled, "embedding_model": input.EmbeddingModel, "top_k": input.TopK, "similarity_threshold": input.Threshold}
	if registry.store != nil {
		if err := registry.store.save(modelState{Models: registry.models, Credentials: registry.credentials, Router: registry.router, SearchTools: registry.searchTools, PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets, ToolPolicies: registry.toolPolicies, MCPSemanticFilter: next, PublicHub: registry.publicHub}); err != nil { registry.mu.Unlock(); stateWriteError(w, err); return }
	}
	registry.mcpSemanticFilter = next
	registry.syncRuntimeToolPolicies()
	registry.mu.Unlock()
	modelJSON(w, http.StatusOK, map[string]any{"message": "MCP Semantic Filter settings updated successfully", "status": "success", "values": registry.mcpSemanticFilter})
}

func cloneAnyMap(values map[string]any) map[string]any { if values == nil { return nil }; result := make(map[string]any, len(values)); for key, value := range values { result[key] = value }; return result }

