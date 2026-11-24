package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type toolPolicy struct {
	Input string `json:"input_policy"`
	Output string `json:"output_policy"`
}

func (registry *modelRegistry) syncRuntimeToolPolicies() {
	policies := make(map[string]runtime.ToolPolicy, len(registry.toolPolicies))
	for name, policy := range registry.toolPolicies { policies[name] = runtime.ToolPolicy{Input: policy.Input, Output: policy.Output} }
	registry.server.ReplaceToolPolicies(policies)
	settings := runtime.MCPSemanticFilterSettings{TopK: 10, SimilarityThreshold: 0.3}
	if value, ok := registry.mcpSemanticFilter["enabled"].(bool); ok { settings.Enabled = value }
	if value, ok := registry.mcpSemanticFilter["top_k"].(int); ok { settings.TopK = value }
	if value, ok := registry.mcpSemanticFilter["top_k"].(float64); ok { settings.TopK = int(value) }
	if value, ok := registry.mcpSemanticFilter["similarity_threshold"].(float64); ok { settings.SimilarityThreshold = value }
	registry.server.ReplaceMCPSemanticFilter(settings)
}

func validToolInputPolicy(value string) bool { return value == "" || slices.Contains([]string{"trusted", "untrusted", "blocked"}, value) }
func validToolOutputPolicy(value string) bool { return value == "" || slices.Contains([]string{"trusted", "untrusted"}, value) }

func (registry *modelRegistry) registerToolPolicyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tool/list", registry.listToolPolicies)
	mux.HandleFunc("GET /v1/tool/policy/options", func(w http.ResponseWriter, r *http.Request) {
		if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
			return
		}
		modelJSON(w, http.StatusOK, map[string]any{
			"input_policies": []map[string]string{
				{"value": "untrusted", "label": "Untrusted", "description": "Tool accepts input from any source."},
				{"value": "trusted", "label": "Trusted", "description": "Tool requires trusted input."},
				{"value": "blocked", "label": "Blocked", "description": "Tool calls are prohibited."},
			},
			"output_policies": []map[string]string{
				{"value": "untrusted", "label": "Untrusted", "description": "Tool output can contain untrusted content."},
				{"value": "trusted", "label": "Trusted", "description": "Tool output is treated as trusted."},
			},
		})
	})
	mux.HandleFunc("POST /v1/tool/policy", registry.updateToolPolicy)
	mux.HandleFunc("GET /v1/tool/{tool_name}/detail", registry.toolDetail)
	mux.HandleFunc("GET /v1/tool/{tool_name}/logs", registry.toolLogs)
	mux.HandleFunc("DELETE /v1/tool/{tool_name}/overrides", registry.deleteToolOverrides)
	mux.HandleFunc("GET /v1/tool/{tool_name...}", registry.toolDetail)
}

func (registry *modelRegistry) listToolPolicies(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) { return }
	registry.mu.Lock(); defer registry.mu.Unlock()
	entries := make([]map[string]any, 0, len(registry.toolPolicies))
	for name, policy := range registry.toolPolicies {
		entries = append(entries, map[string]any{"tool_name": name, "input_policy": policy.Input, "output_policy": policy.Output})
	}
	slices.SortFunc(entries, func(a, b map[string]any) int { return strings.Compare(a["tool_name"].(string), b["tool_name"].(string)) })
	returnJSON(w, http.StatusOK, map[string]any{"tools": entries, "total": len(entries)})
}

func (registry *modelRegistry) toolDetail(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) { return }
	name := r.PathValue("tool_name")
	if name == "" || strings.Contains(name, "..") { modelError(w, http.StatusNotFound, "Tool not found"); return }
	registry.mu.Lock(); policy, ok := registry.toolPolicies[name]; registry.mu.Unlock()
	if !ok { modelError(w, http.StatusNotFound, "Tool not found: "+name); return }
	returnJSON(w, http.StatusOK, map[string]any{"tool": map[string]any{"tool_name": name, "input_policy": policy.Input, "output_policy": policy.Output}, "overrides": []any{}})
}

func (registry *modelRegistry) toolLogs(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) { return }
	page, size := 1, 50
	if value, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && value > 0 { page = value }
	if value, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && value > 0 && value <= 100 { size = value }
	returnJSON(w, http.StatusOK, map[string]any{"logs": []any{}, "total": 0, "page": page, "page_size": size})
}

func (registry *modelRegistry) deleteToolOverrides(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) { return }
	returnJSON(w, http.StatusOK, map[string]any{"tool_name": r.PathValue("tool_name"), "deleted": true})
}

func (registry *modelRegistry) updateToolPolicy(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) { return }
	var input struct { ToolName string `json:"tool_name"`; Input string `json:"input_policy"`; Output string `json:"output_policy"`; TeamID *string `json:"team_id"`; KeyHash *string `json:"key_hash"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 64<<10), &input) != nil || strings.TrimSpace(input.ToolName) == "" || !validToolInputPolicy(input.Input) || !validToolOutputPolicy(input.Output) {
		modelError(w, http.StatusBadRequest, "Invalid tool policy")
		return
	}
	registry.mu.Lock()
	if input.TeamID != nil || input.KeyHash != nil {
		if registry.keys == nil { registry.mu.Unlock(); modelError(w, http.StatusNotImplemented, "Key registry is unavailable"); return }
		registry.keys.mu.Lock()
		next := registry.keys.state
		if input.TeamID != nil && input.KeyHash != nil { registry.keys.mu.Unlock(); registry.mu.Unlock(); modelError(w, http.StatusBadRequest, "Provide either team_id or key_hash, not both"); return }
		if input.TeamID != nil {
			team, exists := next.Teams[*input.TeamID]; if !exists { registry.keys.mu.Unlock(); registry.mu.Unlock(); modelError(w, http.StatusNotFound, "Team not found"); return }
			updated := updateBlockedTool(team.ObjectPermission, input.ToolName, input.Input == "blocked")
			team.ObjectPermission = updated; next.Teams = maps.Clone(next.Teams); next.Teams[*input.TeamID] = team
		} else {
			digest := registry.keys.resolveKey(*input.KeyHash); key, exists := next.Keys[digest]; if !exists { registry.keys.mu.Unlock(); registry.mu.Unlock(); modelError(w, http.StatusNotFound, "Key not found"); return }
			updated := updateBlockedTool(key.ObjectPermission, input.ToolName, input.Input == "blocked")
			key.ObjectPermission = updated; next.Keys = maps.Clone(next.Keys); next.Keys[digest] = key
		}
		if err := registry.keys.commitState(next); err != nil { registry.keys.mu.Unlock(); registry.mu.Unlock(); stateWriteError(w, err); return }
		registry.keys.mu.Unlock(); registry.mu.Unlock()
		returnJSON(w, http.StatusOK, map[string]any{"tool_name": input.ToolName, "input_policy": input.Input, "output_policy": input.Output, "updated": true, "team_id": input.TeamID, "key_hash": input.KeyHash})
		return
	}
	if registry.toolPolicies == nil { registry.toolPolicies = make(map[string]toolPolicy) }
	policy := registry.toolPolicies[input.ToolName]
	if input.Input != "" { policy.Input = input.Input }; if input.Output != "" { policy.Output = input.Output }
	nextPolicies := maps.Clone(registry.toolPolicies); nextPolicies[input.ToolName] = policy
	if registry.store != nil {
		if err := registry.store.save(modelState{Models: registry.models, Credentials: registry.credentials, Router: registry.router, SearchTools: registry.searchTools, PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets, ToolPolicies: nextPolicies, PublicHub: registry.publicHub}); err != nil {
			registry.mu.Unlock(); stateWriteError(w, err); return
		}
	}
	registry.toolPolicies = nextPolicies
	registry.syncRuntimeToolPolicies()
	registry.mu.Unlock()
	returnJSON(w, http.StatusOK, map[string]any{"tool_name": input.ToolName, "input_policy": policy.Input, "output_policy": policy.Output, "updated": true})
}

func returnJSON(w http.ResponseWriter, status int, payload any) { modelJSON(w, status, payload) }

func updateBlockedTool(permission *objectPermission, tool string, blocked bool) *objectPermission {
	var next objectPermission
	if permission != nil { next = *permission; next.BlockedTools = slices.Clone(permission.BlockedTools) }
	if blocked {
		if !slices.Contains(next.BlockedTools, tool) { next.BlockedTools = append(next.BlockedTools, tool) }
	} else {
		next.BlockedTools = slices.DeleteFunc(next.BlockedTools, func(value string) bool { return value == tool })
	}
	return &next
}
