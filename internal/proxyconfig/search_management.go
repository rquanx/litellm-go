package proxyconfig

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type managedSearchTool struct {
	searchToolConfig
	ID        string     `json:"search_tool_id"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func combineSearchTools(static []runtime.SearchTool, managed []managedSearchTool) ([]runtime.SearchTool, error) {
	if len(managed) == 0 {
		return static, nil
	}
	ids, names := make(map[string]bool), make(map[string]bool)
	tools := make([]runtime.SearchTool, 0, len(static)+len(managed))
	for _, entry := range managed {
		if entry.ID == "" || ids[entry.ID] || names[entry.Name] {
			return nil, errors.New("managed search tool IDs and names must be unique")
		}
		tool, err := entry.prepare()
		if err != nil {
			return nil, err
		}
		ids[entry.ID], names[entry.Name] = true, true
		tools = append(tools, tool)
	}
	result := make([]runtime.SearchTool, 0, cap(tools))
	for _, tool := range static {
		if !names[tool.Name] {
			result = append(result, tool)
		}
	}
	return append(result, tools...), nil
}

func (registry *modelRegistry) registerSearchToolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /search_tools/list", registry.listSearchTools)
	mux.HandleFunc("GET /search_tools/{search_tool_id}", registry.getSearchTool)
	mux.HandleFunc("POST /search_tools", registry.changeSearchTool)
	mux.HandleFunc("PUT /search_tools/{search_tool_id}", registry.changeSearchTool)
	mux.HandleFunc("DELETE /search_tools/{search_tool_id}", registry.changeSearchTool)
	mux.HandleFunc("POST /search_tools/test_connection", registry.testSearchToolConnection)
	mux.HandleFunc("GET /search_tools/ui/available_providers", registry.availableSearchProviders)
}

func (registry *modelRegistry) availableSearchProviders(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	entries := make([]map[string]string, 0, len(searchProviderNames))
	for _, name := range slices.Sorted(maps.Keys(searchProviderNames)) {
		entries = append(entries, map[string]string{"provider_name": name, "ui_friendly_name": searchProviderNames[name]})
	}
	modelJSON(w, http.StatusOK, map[string]any{"providers": entries})
}

func (registry *modelRegistry) testSearchToolConnection(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	input := searchToolConfig{Name: "test"}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid search tool configuration")
		return
	}
	tool, err := input.prepare()
	if err != nil {
		modelJSON(w, http.StatusOK, map[string]string{"status": "error", "message": "Invalid search tool configuration", "error_type": "ValueError"})
		return
	}
	count, status := registry.server.TestSearchTool(r.Context(), tool)
	if status != http.StatusOK {
		errorType := "APIError"
		switch status {
		case http.StatusUnauthorized, http.StatusForbidden:
			errorType = "AuthenticationError"
		case http.StatusTooManyRequests:
			errorType = "RateLimitError"
		case http.StatusGatewayTimeout:
			errorType = "Timeout"
		}
		modelJSON(w, http.StatusOK, map[string]string{"status": "error", "message": "Search provider request failed", "error_type": errorType})
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "test_query": "test", "results_count": count,
		"message": "Successfully connected to " + tool.Provider + " search provider"})
}

func searchToolEntry(entry managedSearchTool) map[string]any {
	params := map[string]any{"search_provider": entry.Params.Provider}
	if entry.Params.APIBase != "" {
		params["api_base"] = entry.Params.APIBase
	}
	if entry.Params.EngineID != "" {
		params["search_engine_id"] = entry.Params.EngineID
	}
	if entry.Params.Timeout != 0 {
		params["timeout"] = entry.Params.Timeout
	}
	if entry.Params.Retries != nil {
		params["max_retries"] = *entry.Params.Retries
	}
	result := map[string]any{"search_tool_id": nil, "search_tool_name": entry.Name, "litellm_params": params,
		"search_tool_info": entry.Info, "created_at": entry.CreatedAt, "updated_at": entry.UpdatedAt,
		"is_from_config": entry.ID == ""}
	if entry.ID != "" {
		result["search_tool_id"] = entry.ID
	}
	return result
}

func (registry *modelRegistry) listSearchTools(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	result := make([]map[string]any, 0, len(registry.searchTools)+len(registry.base.staticSearchTools))
	dynamic := make(map[string]bool)
	for _, tool := range registry.searchTools {
		dynamic[tool.Name] = true
	}
	for _, tool := range registry.base.staticSearchTools {
		if !dynamic[tool.Name] {
			result = append(result, searchToolEntry(managedSearchTool{searchToolConfig: tool}))
		}
	}
	for _, tool := range registry.searchTools {
		result = append(result, searchToolEntry(tool))
	}
	modelJSON(w, http.StatusOK, map[string]any{"search_tools": result})
}

func (registry *modelRegistry) getSearchTool(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, tool := range registry.searchTools {
		if tool.ID == r.PathValue("search_tool_id") {
			modelJSON(w, http.StatusOK, searchToolEntry(tool))
			return
		}
	}
	modelError(w, http.StatusNotFound, "Search tool not found")
}

func (registry *modelRegistry) changeSearchTool(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		Tool *searchToolConfig `json:"search_tool"`
	}
	if r.Method != http.MethodDelete &&
		(decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.Tool == nil) {
		modelError(w, http.StatusBadRequest, "Invalid search tool configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := slices.Clone(registry.searchTools)
	now := time.Now().UTC()
	var entry managedSearchTool
	if r.Method == http.MethodPost {
		entry = managedSearchTool{searchToolConfig: *input.Tool, ID: newEntityID(), CreatedAt: &now, UpdatedAt: &now}
		next = append(next, entry)
	} else {
		id := r.PathValue("search_tool_id")
		index := slices.IndexFunc(next, func(tool managedSearchTool) bool { return tool.ID == id })
		if index < 0 {
			modelError(w, http.StatusNotFound, "Search tool not found")
			return
		}
		entry = next[index]
		if r.Method == http.MethodDelete {
			next = slices.Delete(next, index, index+1)
		} else {
			if input.Tool.Params.APIKey == "" && entry.Params.Provider == input.Tool.Params.Provider {
				input.Tool.Params.APIKey = entry.Params.APIKey
			}
			entry.searchToolConfig, entry.UpdatedAt = *input.Tool, &now
			next[index] = entry
		}
	}
	if !registry.publishConfiguration(w, registry.models, registry.credentials, registry.router, next) {
		return
	}
	if r.Method == http.MethodDelete {
		modelJSON(w, http.StatusOK, map[string]string{
			"message": "Search tool " + entry.ID + " deleted successfully", "search_tool_name": entry.Name,
		})
		return
	}
	modelJSON(w, http.StatusOK, searchToolEntry(entry))
}
