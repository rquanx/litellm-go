package proxyconfig

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

const mcpToolsetPath = "/v1/mcp/toolset"

func (registry *modelRegistry) registerMCPToolsetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+mcpToolsetPath, registry.readMCPToolset)
	mux.HandleFunc("GET "+mcpToolsetPath+"/{toolset_id}", registry.readMCPToolset)
	mux.HandleFunc("POST "+mcpToolsetPath, registry.writeMCPToolset)
	mux.HandleFunc("PUT "+mcpToolsetPath, registry.writeMCPToolset)
	mux.HandleFunc("DELETE "+mcpToolsetPath+"/{toolset_id}", registry.writeMCPToolset)
}

func (registry *modelRegistry) readMCPToolset(w http.ResponseWriter, r *http.Request) {
	scope, valid := registry.server.MCPToolsetScope(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Runtime token required")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if id := r.PathValue("toolset_id"); id != "" {
		if scope != nil && !slices.Contains(scope, id) {
			modelError(w, http.StatusForbidden, "MCP toolset access denied")
			return
		}
		for _, set := range registry.mcpToolsets {
			if set.ID == id {
				modelJSON(w, http.StatusOK, set)
				return
			}
		}
		modelError(w, http.StatusNotFound, "MCP toolset not found")
		return
	}
	visible := make([]runtime.MCPToolset, 0)
	for _, set := range registry.mcpToolsets {
		if scope == nil || slices.Contains(scope, set.ID) {
			visible = append(visible, set)
		}
	}
	modelJSON(w, http.StatusOK, visible)
}

func (registry *modelRegistry) writeMCPToolset(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		ID string `json:"toolset_id"`
		Name *string `json:"toolset_name"`
		Description *string `json:"description"`
		Tools *[]runtime.MCPToolsetEntry `json:"tools"`
	}
	if r.Method != http.MethodDelete && decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid MCP toolset")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := slices.Clone(registry.mcpToolsets)
	now := time.Now().UTC()
	status := http.StatusOK
	var entry runtime.MCPToolset
	switch r.Method {
	case http.MethodPost:
		entry = runtime.MCPToolset{ID: newEntityID(), CreatedAt: now, Tools: []runtime.MCPToolsetEntry{}}
		next = append(next, entry)
		status = http.StatusCreated
	case http.MethodPut, http.MethodDelete:
		id := input.ID
		if r.Method == http.MethodDelete {
			id = r.PathValue("toolset_id")
		}
		index := slices.IndexFunc(next, func(set runtime.MCPToolset) bool { return set.ID == id })
		if index < 0 {
			modelError(w, http.StatusNotFound, "MCP toolset not found")
			return
		}
		entry = next[index]
		next = slices.Delete(next, index, index+1)
		if r.Method == http.MethodDelete {
			status = http.StatusAccepted
		} else {
			next = append(next, entry)
		}
	}
	if r.Method != http.MethodDelete {
		if input.Name != nil {
			entry.Name = strings.TrimSpace(*input.Name)
		}
		if input.Description != nil {
			entry.Description = *input.Description
		}
		if input.Tools != nil {
			entry.Tools = *input.Tools
		}
		entry.UpdatedAt = now
		if entry.Name == "" || strings.Contains(entry.Name, "/") || len(entry.Tools) == 0 {
			modelError(w, http.StatusBadRequest, "Supply a toolset name and at least one tool")
			return
		}
		for _, other := range next[:len(next)-1] {
			if other.Name == entry.Name {
				modelError(w, http.StatusConflict, "MCP toolset name already exists")
				return
			}
		}
		for _, tool := range entry.Tools {
			if tool.ServerID == "" || strings.TrimSpace(tool.Name) == "" {
				modelError(w, http.StatusBadRequest, "Each tool requires a server ID and name")
				return
			}
		}
		next[len(next)-1] = entry
	}
	if registry.store != nil {
		state := modelState{Models: registry.models, Credentials: registry.credentials, Router: registry.router,
			SearchTools: registry.searchTools, PassThrough: registry.passThrough, MCPServers: registry.mcpServers, MCPToolsets: next, PublicHub: registry.publicHub}
		if err := registry.store.save(state); err != nil {
			stateWriteError(w, err)
			return
		}
	}
	registry.mcpToolsets = next
	registry.server.ReplaceMCPToolsets(next)
	if r.Method == http.MethodDelete {
		modelJSON(w, status, map[string]string{"message": "MCP toolset deleted"})
		return
	}
	modelJSON(w, status, entry)
}
