package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
)

//go:embed mcp_registry.json
var mcpRegistryJSON []byte

//go:embed openapi_registry.json
var openAPIMCPRegistryJSON json.RawMessage

var curatedMCP = func() []map[string]any {
	var catalog struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(mcpRegistryJSON, &catalog); err != nil {
		panic(err)
	}
	return catalog.Servers
}()

var curatedMCPCategories = func() []string {
	categories := make(map[string]struct{})
	for _, entry := range curatedMCP {
		category, ok := entry["category"].(string)
		if !ok {
			category = "Other"
		}
		categories[category] = struct{}{}
	}
	return slices.Sorted(maps.Keys(categories))
}()

func (registry *modelRegistry) discoverMCPServers(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
		return
	}
	query := r.URL.Query()
	category, term := query.Get("category"), strings.ToLower(query.Get("query"))
	servers := make([]map[string]any, 0, len(curatedMCP))
	for _, entry := range curatedMCP {
		value, _ := entry["category"].(string)
		if category != "" && value != category {
			continue
		}
		if term != "" && !slices.ContainsFunc([]string{"name", "title", "description"}, func(field string) bool {
			value, _ := entry[field].(string)
			return strings.Contains(strings.ToLower(value), term)
		}) {
			continue
		}
		servers = append(servers, entry)
	}
	modelJSON(w, http.StatusOK, map[string]any{"servers": servers, "categories": curatedMCPCategories})
}

func (registry *modelRegistry) openAPIMCPRegistry(w http.ResponseWriter, r *http.Request) {
	if registry.keys.adminViewer(r) || registry.allowed(w, r) {
		modelJSON(w, http.StatusOK, openAPIMCPRegistryJSON)
	}
}
