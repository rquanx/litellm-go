package proxyconfig

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

const passThroughManagementPath = "/config/pass_through_endpoint"

func (registry *modelRegistry) loadPassThrough() error {
	if len(registry.passThrough) == 0 && len(registry.base.staticPassThrough) == 0 {
		return nil
	}
	routes, err := registry.compilePassThrough(registry.passThrough)
	if err == nil {
		registry.server.ReplacePassThrough(routes)
	}
	return err
}

func (registry *modelRegistry) compilePassThrough(entries []passThroughConfig) ([]runtime.PassThroughRoute,error) {
	if err:=validatePassThroughIDs(entries);err!=nil {
		return nil,err
	}
	return preparePassThrough(append(slices.Clone(registry.base.staticPassThrough),entries...))
}

func (registry *modelRegistry) registerPassThroughRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+passThroughManagementPath, registry.listPassThrough)
	mux.HandleFunc("GET "+passThroughManagementPath+"/team/{team_id}", registry.listPassThrough)
	mux.HandleFunc("POST "+passThroughManagementPath, registry.changePassThrough)
	mux.HandleFunc("POST "+passThroughManagementPath+"/{endpoint_id}", registry.changePassThrough)
	mux.HandleFunc("DELETE "+passThroughManagementPath, registry.changePassThrough)
}

func publicPassThrough(entry passThroughConfig, fromConfig bool) map[string]any {
	address := entry.Target
	if target, err := url.Parse(address); err == nil && target.RawQuery != "" {
		target.RawQuery = ""
		address = target.String()
	}
	headers := make(map[string]string, len(entry.Headers))
	for name := range entry.Headers {
		headers[name] = "****"
	}
	query := make(map[string]string, len(entry.DefaultQuery))
	for name := range entry.DefaultQuery {
		query[name] = "****"
	}
	return map[string]any{"id": entry.ID, "path": entry.Path, "target": address, "headers": headers,
		"auth": entry.Auth == nil || *entry.Auth, "methods": entry.Methods, "cost_per_request": entry.Cost,
		"include_subpath": entry.IncludeSubpath, "default_query_params": query, "is_from_config": fromConfig}
}

func (registry *modelRegistry) listPassThrough(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
		return
	}
	var metadata map[string]any
	teamID := r.PathValue("team_id")
	if teamID == "" {
		teamID = r.URL.Query().Get("team_id")
	}
	if teamID != "" {
		if registry.keys == nil {
			modelError(w, http.StatusNotFound, "Team not found")
			return
		}
		registry.keys.mu.RLock()
		entry, exists := registry.keys.state.Teams[teamID]
		registry.keys.mu.RUnlock()
		if !exists {
			modelError(w, http.StatusNotFound, "Team not found")
			return
		}
		metadata = entry.Metadata
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	results := make([]map[string]any, 0, len(registry.base.staticPassThrough)+len(registry.passThrough))
	id := r.URL.Query().Get("endpoint_id")
	if id == "" {
		for _, entry := range registry.base.staticPassThrough {
			if teamListsPassThrough(metadata, entry.Path) {
				results = append(results, publicPassThrough(entry, true))
			}
		}
	}
	for _, entry := range registry.passThrough {
		if (id == "" || id == entry.ID) && teamListsPassThrough(metadata, entry.Path) {
			results = append(results, publicPassThrough(entry, false))
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"endpoints": results})
}

func (registry *modelRegistry) changePassThrough(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var entry passThroughConfig
	id := r.PathValue("endpoint_id")
	if r.Method == http.MethodDelete {
		id = r.URL.Query().Get("endpoint_id")
		if id == "" {
			modelError(w, http.StatusBadRequest, "Supply a pass-through endpoint ID")
			return
		}
	} else if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &entry); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid pass-through configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := slices.Clone(registry.passThrough)
	if id == "" {
		entry.ID = newEntityID()
		next = append(next, entry)
	} else {
		index := slices.IndexFunc(next, func(endpoint passThroughConfig) bool { return endpoint.ID == id })
		if index < 0 {
			modelError(w, http.StatusNotFound, "Pass-through endpoint not found")
			return
		}
		if r.Method == http.MethodDelete {
			next = slices.Delete(next, index, index+1)
		} else {
			entry.ID = id
			next[index] = entry
		}
	}
	routes, err := preparePassThrough(append(slices.Clone(registry.base.staticPassThrough), next...))
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := registry.savePassThrough(next); err != nil {
		stateWriteError(w, err)
		return
	}
	registry.passThrough = next
	registry.server.ReplacePassThrough(routes)
	results := make([]map[string]any, 0, 1)
	if r.Method != http.MethodDelete {
		results = append(results, publicPassThrough(entry, false))
	}
	modelJSON(w, http.StatusOK, map[string]any{"endpoints": results})
}

func (registry *modelRegistry) savePassThrough(entries []passThroughConfig) error {
	if err := validatePassThroughIDs(entries); err != nil {
		return err
	}
	if registry.store == nil {
		return nil
	}
	return registry.store.save(modelState{AllowedIPs: registry.allowedIPs, Models: registry.models, Credentials: registry.credentials,
		Router: registry.router, SearchTools: registry.searchTools, PassThrough: entries,
		MCPServers: registry.mcpServers, MCPToolsets: registry.mcpToolsets, ToolPolicies: registry.toolPolicies, MCPSemanticFilter: registry.mcpSemanticFilter, PublicHub: registry.publicHub})
}

func validatePassThroughIDs(entries []passThroughConfig) error {
	ids := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.ID) == "" || ids[entry.ID] {
			return errors.New("invalid pass-through endpoint ID")
		}
		ids[entry.ID] = true
	}
	return nil
}

const passThroughRoutesMetadata = "allowed_passthrough_routes"

func withPassThroughRoutes(metadata map[string]any, routes []string) map[string]any {
	next := make(map[string]any, len(metadata)+1)
	maps.Copy(next, metadata)
	next[passThroughRoutesMetadata] = slices.Clone(routes)
	return next
}

func passThroughRoutes(metadata map[string]any) []string {
	switch routes := metadata[passThroughRoutesMetadata].(type) {
	case []string:
		return routes
	case []any:
		result := make([]string, 0, len(routes))
		for _, item := range routes {
			if route, ok := item.(string); ok {
				result = append(result, route)
			}
		}
		return result
	default:
		return nil
	}
}

func teamListsPassThrough(metadata map[string]any, path string) bool {
	return metadata[passThroughRoutesMetadata] == nil || slices.Contains(passThroughRoutes(metadata), path)
}

func (keys *KeyRegistry) AuthorizePassThrough(token, path string) bool {
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	if _, _, ok := keys.authorizeLocked(token); !ok {
		return false
	}
	key := keys.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key = keys.state.Keys[key.RotationTarget]
	}
	teamID := key.TeamID
	if teamID == "" && key.ProjectID != "" {
		teamID = keys.state.Projects[key.ProjectID].TeamID
	}
	routes := passThroughRoutes(key.Metadata)
	if len(routes) == 0 {
		routes = passThroughRoutes(keys.state.Teams[teamID].Metadata)
	}
	return slices.ContainsFunc(routes, func(route string) bool {
		return route != "" && (path == route || strings.HasPrefix(path, route+"/"))
	})
}
