package runtime

import (
	"net/http"
	"slices"
	"time"
)

type MCPToolset struct {
	ID          string            `json:"toolset_id"`
	Name        string            `json:"toolset_name"`
	Description string            `json:"description"`
	Tools       []MCPToolsetEntry `json:"tools"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type MCPToolsetEntry struct {
	ServerID string `json:"server_id"`
	Name     string `json:"tool_name"`
}

func (s *Server) ReplaceMCPToolsets(toolsets []MCPToolset) {
	s.mu.Lock()
	s.mcpToolsets = toolsets
	s.mu.Unlock()
}

func (s *Server) serveMCPForToolset(w http.ResponseWriter, r *http.Request, selector string) {
	grants, allowed, valid := s.mcpToolsetGrants(r)
	if !valid {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return
	}
	s.mu.RLock()
	toolsets := s.mcpToolsets
	s.mu.RUnlock()
	for _, set := range toolsets {
		if (set.Name != selector && set.ID != selector) || (grants != nil && !slices.Contains(grants, set.ID)) {
			continue
		}
		// A grant gives access only to the tools explicitly saved in the toolset.
		filter := func(server MCPServer, tool string) bool {
			for _, entry := range set.Tools {
				if entry.ServerID == server.ID && (tool == "" || entry.Name == tool) && allowed(server, tool) {
					return true
				}
			}
			return false
		}
		s.dispatchMCP(w, r, filter, true)
		return
	}
	writeError(w, http.StatusNotFound, "toolset_not_found", "MCP toolset not found")
}

func (s *Server) MCPToolsetScope(r *http.Request) ([]string, bool) {
	grants, _, valid := s.mcpToolsetGrants(r)
	return grants, valid
}

func (s *Server) mcpToolsetGrants(r *http.Request) ([]string, MCPAccess, bool) {
	if s.authorized(r) {
		return nil, func(MCPServer, string) bool { return true }, true
	}
	if s.config.AuthorizeMCPToolset != nil {
		grants, allowed, valid := s.config.AuthorizeMCPToolset(ProxyCredential(r))
		return append([]string{}, grants...), allowed, valid
	}
	_, valid := s.mcpAccess(r)
	return []string{}, nil, valid
}
