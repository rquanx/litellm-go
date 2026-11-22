package proxyconfig

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

const mcpServerPath = "/v1/mcp/server"

type mcpServerInput struct {
	mcpServerConfig
	Name string `json:"server_name"`
}

func (registry *modelRegistry) compileMCP(managed map[string]mcpServerConfig) ([]runtime.MCPServer, error) {
	combined := make(map[string]mcpServerConfig, len(registry.base.staticMCPServers)+len(managed))
	maps.Copy(combined, registry.base.staticMCPServers)
	for name, server := range managed {
		if _, exists := combined[name]; exists || server.ServerID == "" {
			return nil, fmt.Errorf("invalid or duplicate managed MCP server %q", name)
		}
		if server.Command != "" || server.Transport == "stdio" {
			return nil, fmt.Errorf("stdio MCP server %q requires local configuration", name)
		}
		combined[name] = server
	}
	return prepareMCPServers(combined)
}

func (registry *modelRegistry) registerMCPRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /public/mcp_hub", registry.publicMCPHub)
	mux.HandleFunc("GET "+mcpServerPath, registry.readMCPServer)
	mux.HandleFunc("GET "+mcpServerPath+"/{server_id}", registry.readMCPServer)
	mux.HandleFunc("POST "+mcpServerPath, registry.writeMCPServer)
	mux.HandleFunc("PUT "+mcpServerPath, registry.writeMCPServer)
	mux.HandleFunc("DELETE "+mcpServerPath+"/{server_id}", registry.writeMCPServer)
	mux.HandleFunc("POST /mcp-rest/test/connection", registry.testMCPServer)
	mux.HandleFunc("POST /mcp-rest/test/tools/list", registry.testMCPServer)
}

func (registry *modelRegistry) testMCPServer(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input mcpServerInput
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.Transport == "stdio" || input.Command != "" {
		modelError(w, http.StatusBadRequest, "Invalid remote MCP server configuration")
		return
	}
	if input.Name == "" {
		input.Name = "test"
	}
	if input.Transport == "" {
		input.Transport = "sse"
	}
	servers, err := prepareMCPServers(map[string]mcpServerConfig{input.Name: input.mcpServerConfig})
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	listTools := r.URL.Path == "/mcp-rest/test/tools/list"
	tools, err := registry.server.ProbeMCP(r.Context(), servers[0], listTools)
	if err != nil {
		modelError(w, http.StatusBadGateway, "MCP server probe failed")
		return
	}
	if listTools {
		if tools == nil {
			tools = []map[string]any{}
		}
		for _, tool := range tools {
			delete(tool, "mcp_info")
		}
		modelJSON(w, http.StatusOK, map[string]any{"tools": tools, "error": nil, "message": "Successfully retrieved tools"})
		return
	}
	modelJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func mcpServerEntry(name string, server mcpServerConfig) mcpServerInput {
	server.Headers, server.Env = nil, nil
	if address, err := url.Parse(server.URL); err == nil && address.RawQuery != "" {
		address.RawQuery = ""
		server.URL = address.String()
	}
	if server.ServerID == "" {
		server.ServerID = name
	}
	return mcpServerInput{mcpServerConfig: server, Name: name}
}

func (registry *modelRegistry) readMCPServer(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entries := make([]mcpServerInput, 0)
	for _, group := range []map[string]mcpServerConfig{registry.base.staticMCPServers, registry.mcpServers} {
		for _, name := range slices.Sorted(maps.Keys(group)) {
			entries = append(entries, mcpServerEntry(name, group[name]))
		}
	}
	if id := r.PathValue("server_id"); id != "" {
		for _, entry := range entries {
			if id == entry.ServerID {
				modelJSON(w, http.StatusOK, entry)
				return
			}
		}
		modelError(w, http.StatusNotFound, "MCP server not found")
		return
	}
	modelJSON(w, http.StatusOK, entries)
}

func (registry *modelRegistry) writeMCPServer(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input mcpServerInput
	if r.Method != http.MethodDelete && decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid MCP server configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := make(map[string]mcpServerConfig, len(registry.mcpServers)+1)
	maps.Copy(next, registry.mcpServers)
	if r.Method == http.MethodPost {
		input.ServerID = newEntityID()
	} else {
		id := input.ServerID
		if r.Method == http.MethodDelete {
			id = r.PathValue("server_id")
		}
		var existing string
		for name, entry := range next {
			if entry.ServerID == id {
				existing = name
				if r.Method == http.MethodPut && input.Headers == nil {
					input.Headers = entry.Headers
				}
				break
			}
		}
		if existing == "" {
			modelError(w, http.StatusNotFound, "MCP server not found")
			return
		}
		delete(next, existing)
		if input.Name == "" {
			input.Name = existing
		}
	}
	if r.Method != http.MethodDelete {
		if _, exists := next[input.Name]; exists {
			modelError(w, http.StatusConflict, "MCP server name already exists")
			return
		}
		if input.Transport == "" {
			input.Transport = "sse"
		}
		next[input.Name] = input.mcpServerConfig
	}
	servers, err := registry.compileMCP(next)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if registry.store != nil {
		if err := registry.store.save(modelState{Models: registry.models, Credentials: registry.credentials,
			Router: registry.router, SearchTools: registry.searchTools, PassThrough: registry.passThrough,
			MCPServers: next, MCPToolsets: registry.mcpToolsets, PublicHub: registry.publicHub}); err != nil {
			stateWriteError(w, err)
			return
		}
	}
	registry.mcpServers = next
	registry.server.ReplaceMCPServers(servers)
	if r.Method == http.MethodDelete {
		modelJSON(w, http.StatusOK, map[string]string{"message": "MCP server deleted successfully"})
		return
	}
	modelJSON(w, http.StatusOK, mcpServerEntry(input.Name, input.mcpServerConfig))
}
