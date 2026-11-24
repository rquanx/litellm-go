package runtime

import (
	"maps"
	"strings"
)

func (s *Server) ReplaceToolPolicies(policies map[string]ToolPolicy) {
	s.mu.Lock()
	s.toolPolicies = maps.Clone(policies)
	s.mu.Unlock()
}

func (s *Server) ReplaceMCPSemanticFilter(settings MCPSemanticFilterSettings) {
	s.mu.Lock(); s.mcpSemanticFilter = settings; s.mu.Unlock()
}

func (s *Server) toolPolicyAllowed(server MCPServer, tool string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, name := range []string{server.Name + "-" + tool, tool, server.Name + "." + tool} {
		if policy, ok := s.toolPolicies[name]; ok && strings.EqualFold(policy.Input, "blocked") {
			return false
		}
	}
	return true
}

func (s *Server) toolPolicy(server MCPServer, tool string) ToolPolicy {
	s.mu.RLock(); defer s.mu.RUnlock()
	for _, name := range []string{server.Name + "-" + tool, tool, server.Name + "." + tool} {
		if policy, ok := s.toolPolicies[name]; ok { return policy }
	}
	return ToolPolicy{}
}

func (s *Server) semanticFilterSettings() MCPSemanticFilterSettings { s.mu.RLock(); defer s.mu.RUnlock(); return s.mcpSemanticFilter }
