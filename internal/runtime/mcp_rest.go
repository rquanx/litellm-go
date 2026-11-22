package runtime

import (
	"encoding/json"
	"net/http"
	"path"
	"slices"
	"strings"
)

type MCPAccess func(MCPServer, string) bool

func (s *Server) ReplaceMCPServers(servers []MCPServer) {
	s.mu.Lock()
	s.mcpServers = servers
	s.mu.Unlock()
}

func (s *Server) currentMCPServers() []MCPServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mcpServers
}

func (s *Server) mcpAccess(r *http.Request) (MCPAccess, bool) {
	if s.authorized(r) {
		return func(MCPServer, string) bool { return true }, true
	}
	token := ProxyCredential(r)
	if s.config.AuthorizeMCP != nil {
		return s.config.AuthorizeMCP(token)
	}
	if s.config.AuthorizeKey != nil {
		_, valid := s.config.AuthorizeKey(token)
		if valid {
			return func(server MCPServer, _ string) bool { return server.AllowAllKeys }, true
		}
	}
	return nil, false
}

func (s *Server) serveMCPREST(w http.ResponseWriter, r *http.Request) bool {
	list := r.Method == http.MethodGet && r.URL.Path == "/mcp-rest/tools/list"
	call := r.Method == http.MethodPost && r.URL.Path == "/mcp-rest/tools/call"
	groupList := r.Method == http.MethodGet && r.URL.Path == "/v1/mcp/access_groups"
	protocolList := r.Method == http.MethodGet && r.URL.Path == "/v1/mcp/tools"
	if !list && !call && !groupList && !protocolList {
		return false
	}
	access, valid := s.mcpAccess(r)
	if !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return true
	}
	if groupList {
		groups := make([]string, 0)
		for _, server := range s.currentMCPServers() {
			if access(server, "") {
				groups = append(groups, server.AccessGroups...)
			}
		}
		slices.Sort(groups)
		writeJSON(w, http.StatusOK, map[string]any{"access_groups": slices.Compact(groups)})
		return true
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return true
	}
	if protocolList {
		writeJSON(w, http.StatusOK, map[string]any{"tools": s.listProtocolTools(r, access)})
	} else if list {
		s.listMCPTools(w, r, access)
	} else {
		s.callMCPTool(w, r, access)
	}
	return true
}

func (s *Server) listMCPTools(w http.ResponseWriter, r *http.Request, access MCPAccess) {
	selector := r.URL.Query().Get("server_id")
	tools := make([]map[string]any, 0)
	var listed bool
	for _, server := range s.currentMCPServers() {
		if !access(server, "") || (selector != "" && !server.matches(selector)) {
			continue
		}
		listed = true
		results, err := s.mcpTools(r.Context(), server)
		if err != nil {
			if selector != "" {
				writeJSON(w, http.StatusOK, map[string]any{"tools": tools, "error": "server_error", "message": "Could not list MCP tools"})
				return
			}
			continue
		}
		for _, tool := range results {
			if access(server, tool["name"].(string)) {
				tools = append(tools, tool)
			}
		}
	}
	if !listed && selector != "" {
		writeJSON(w, http.StatusOK, map[string]any{"tools": tools, "error": "server_not_found", "message": "MCP server not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools, "error": nil, "message": nil})
}

func (s *Server) callMCPTool(w http.ResponseWriter, r *http.Request, access MCPAccess) {
	var input struct {
		ServerID string `json:"server_id"`
		Name string `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	body, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil || json.Unmarshal(body, &input) != nil || input.ServerID == "" || input.Name == "" {
		writeError(w, http.StatusBadRequest, "missing_parameter", "Supply server_id and name")
		return
	}
	for _, server := range s.currentMCPServers() {
		if !server.matches(input.ServerID) {
			continue
		}
		name := input.Name
		for _, prefix := range []string{server.Alias, server.Name, server.ID} {
			if prefix != "" && strings.HasPrefix(name, prefix+"-") {
				name = strings.TrimPrefix(name, prefix+"-")
				break
			}
		}
		if name == "" || !access(server, name) || !server.allowsTool(name) || !server.allowsArguments(name, input.Arguments) {
			writeError(w, http.StatusForbidden, "tool_not_allowed", "MCP tool access denied")
			return
		}
		session, err := s.openMCP(r.Context(), server)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "Could not open MCP session")
			return
		}
		defer session.close()
		if input.Arguments == nil {
			input.Arguments = make(map[string]json.RawMessage)
		}
		response, err := session.rpc("tools/call", map[string]any{"name": name, "arguments": input.Arguments})
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "MCP tool call failed")
			return
		}
		writeRawResponse(w, http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, response)
		return
	}
	writeError(w, http.StatusNotFound, "server_not_found", "MCP server not found")
}

func (server MCPServer) matches(id string) bool {
	return id == server.ID || id == server.Name || (server.Alias != "" && id == server.Alias)
}

func (server MCPServer) allowsTool(name string) bool {
	qualified := server.Name + "-" + name
	if len(server.AllowedTools) != 0 {
		return MCPToolMatches(server.AllowedTools, name) || MCPToolMatches(server.AllowedTools, qualified)
	}
	return !MCPToolMatches(server.DisallowedTools, name) && !MCPToolMatches(server.DisallowedTools, qualified)
}

func (server MCPServer) allowsArguments(name string, args map[string]json.RawMessage) bool {
	allowed := server.AllowedParams[server.Name+"-"+name]
	if len(allowed) == 0 {
		allowed = server.AllowedParams[name]
	}
	if len(allowed) == 0 {
		return true
	}
	for argument := range args {
		if !slices.Contains(allowed, argument) {
			return false
		}
	}
	return true
}

func MCPToolMatches(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if match, _ := path.Match(pattern, name); pattern == name || match {
			return true
		}
	}
	return false
}
