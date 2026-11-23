package proxyconfig

import (
	"maps"
	"net/http"
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
	registry.mu.Lock()
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
			if !entry.Public && !slices.Contains(registry.base.publicMCPServers, id) &&
				!slices.Contains(registry.base.publicMCPServers, name) {
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
	registry.mu.Unlock()
	modelJSON(w, http.StatusOK, entries)
}
