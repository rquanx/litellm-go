package proxyconfig

import (
	"maps"
	"net/http"
	"strings"
	"net/url"
	"slices"
)

type publicMCPServer struct {
	ID         string         `json:"server_id"`
	Name       string         `json:"name"`
	Alias      *string        `json:"alias"`
	ServerName string         `json:"server_name"`
	URL        *string        `json:"url"`
	Transport  string         `json:"transport"`
	SpecPath   *string        `json:"spec_path"`
	AuthType   *string        `json:"auth_type"`
	Info       map[string]any `json:"mcp_info"`
}

func (registry *modelRegistry) publicMCPHub(w http.ResponseWriter, _ *http.Request) {
	modelJSON(w, http.StatusOK, registry.publicMCPEntries())
}

func (registry *modelRegistry) publicMCPEntries() []publicMCPServer {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	publicIDs := registry.publicHub.MCPServers
	if publicIDs == nil {
		publicIDs = registry.base.publicMCPServers
	}
	entries := make([]publicMCPServer, 0)
	for index, group := range []map[string]mcpServerConfig{registry.base.staticMCPServers, registry.mcpServers} {
		for _, name := range slices.Sorted(maps.Keys(group)) {
			entry := group[name]
			if !entry.mcpApproved() {
				continue
			}
			resolved, err := resolveEnv(entry.URL)
			if err != nil {
				continue
			}
			id := mcpPublicID(name, resolved, entry)
			if !entry.Public && !slices.Contains(publicIDs, id) && !slices.Contains(publicIDs, name) {
				continue
			}
			item := publicMCPServer{ID: id, Name: name, ServerName: name, Transport: mcpPublicTransport(entry.Transport), Info: entry.PublicInfo}
			if index == 0 {
				item.Info = make(map[string]any, len(entry.PublicInfo)+2)
				maps.Copy(item.Info, entry.PublicInfo)
				if _, exists := item.Info["server_name"]; !exists {
					item.Info["server_name"] = name
				}
				if _, exists := item.Info["description"]; !exists && entry.Description != "" {
					item.Info["description"] = entry.Description
				}
			}
			if entry.Alias != "" {
				item.Name, item.Alias = entry.Alias, &entry.Alias
			}
			if entry.ImportedAuthType != "" {
				item.AuthType = &entry.ImportedAuthType
			}
			if address, err := url.Parse(resolved); err == nil {
				address.RawQuery, address.ForceQuery = "", false
				display := address.String()
				item.URL = &display
			}
			entries = append(entries, item)
		}
	}
	return entries
}

func (registry *modelRegistry) updatePublicMCPServers(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"mcp_server_ids"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.IDs == nil {
		modelError(w, http.StatusUnprocessableEntity, "Supply mcp_server_ids")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	active := make(map[string]bool)
	for _, group := range []map[string]mcpServerConfig{registry.base.staticMCPServers, registry.mcpServers} {
		for name, entry := range group {
			if address, err := resolveEnv(entry.URL); err == nil && entry.mcpApproved() {
				active[mcpPublicID(name, address, entry)] = true
			}
		}
	}
	for _, id := range input.IDs {
		if !active[id] {
			modelJSON(w, http.StatusNotFound, map[string]string{"detail": "MCP Server with ID "+id+" not found"})
			return
		}
	}
	next := registry.publicHub
	next.MCPServers = input.IDs
	if registry.savePublicHub(w, next) {
		modelJSON(w, http.StatusAccepted, map[string]any{
			"message": "Successfully updated public mcp servers",
			"public_mcp_servers": input.IDs,
			"updated_by": "default_user_id",
		})
	}
}

type mcpRegistryServer struct {
	Name string `json:"name"`
	Title string `json:"title"`
	Description string `json:"description"`
	Version string `json:"version"`
	Remotes []mcpRegistryRemote `json:"remotes"`
}

type mcpRegistryRemote struct {
	Type string `json:"type"`
	URL string `json:"url"`
}

func (registry *modelRegistry) publicMCPRegistry(w http.ResponseWriter, r *http.Request) {
	if !registry.base.enableMCPRegistry {
		modelJSON(w, http.StatusNotFound, map[string]string{"detail": "MCP registry is not enabled"})
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	type item struct {
		Server mcpRegistryServer `json:"server"`
	}
	entry := func(name, description, path string) item {
		return item{Server: mcpRegistryServer{
			Name: name, Title: name, Description: description, Version: "1.0.0",
			Remotes: []mcpRegistryRemote{{Type: "streamable-http", URL: scheme+"://"+r.Host+path}},
		}}
	}
	items := []item{entry("litellm-mcp-server", "MCP Server for LiteLLM", "/mcp")}
	public := registry.publicMCPEntries()
	slices.SortFunc(public, func(a, b publicMCPServer) int { return strings.Compare(a.Name, b.Name) })
	for _, server := range public {
		items = append(items, entry(server.Name, server.Name, "/"+url.PathEscape(server.Name)+"/mcp"))
	}
	modelJSON(w, http.StatusOK, map[string]any{"servers": items})
}
