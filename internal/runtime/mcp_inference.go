package runtime

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const localMCPToolURL = "litellm_proxy/mcp"

var mcpFunctionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (s *Server) expandMCPInferenceTools(r *http.Request, endpoint string, fields map[string]any) error {
	tools, _ := fields["tools"].([]any)
	var selected []map[string]any
	expanded := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		address, _ := tool["server_url"].(string)
		if tool["type"] == "mcp" && (address == localMCPToolURL || strings.HasPrefix(address, localMCPToolURL+"/")) {
			selected = append(selected, tool)
		} else {
			expanded = append(expanded, raw)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	access, ok := s.mcpAccess(r)
	if !ok {
		return errors.New("MCP tool discovery requires access to the selected servers")
	}
	used := make(map[string]bool)
	for _, raw := range expanded {
		tool, _ := raw.(map[string]any)
		if function, ok := tool["function"].(map[string]any); ok {
			tool = function
		}
		if name, _ := tool["name"].(string); name != "" {
			used[name] = true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	discovered := make(map[string][]map[string]any)
	for _, declaration := range selected {
		address, _ := declaration["server_url"].(string)
		selector := strings.TrimPrefix(strings.TrimPrefix(address, localMCPToolURL), "/")
		names := strings.Split(selector, ",")
		if declaration["require_approval"] == "never" {
			return errors.New("Automatic MCP tool execution is unavailable")
		}
		for _, server := range s.currentMCPServers() {
			if !access(server, "") || !mcpInferenceServerMatch(server, names) {
				continue
			}
			entries, cached := discovered[server.ID]
			if !cached {
				authenticated := s.mcpRequestServer(r.WithContext(ctx), server)
				select {
				case s.sem <- struct{}{}:
				case <-ctx.Done():
					return errors.New("MCP tool discovery timed out")
				}
				var err error
				entries, err = s.mcpTools(ctx, authenticated)
				<-s.sem
				if err != nil {
					return errors.New("Could not list tools from the selected MCP server")
				}
				discovered[server.ID] = entries
			}
			for _, entry := range entries {
				name, _ := entry["name"].(string)
				fullName := server.Name + "-" + name
				if !access(server, name) || !mcpFunctionName.MatchString(fullName) ||
					!mcpInferenceToolAllowed(declaration["allowed_tools"], name, fullName) || used[fullName] {
					continue
				}
				used[fullName] = true
				description, _ := entry["description"].(string)
				schema, _ := entry["inputSchema"].(map[string]any)
				parameters := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
				for key, value := range schema {
					parameters[key] = value
				}
				function := map[string]any{"name": fullName, "description": description, "parameters": parameters, "strict": false}
				if endpoint == "chat-completions" {
					expanded = append(expanded, map[string]any{"type": "function", "function": function})
				} else {
					function["type"] = "function"
					expanded = append(expanded, function)
				}
			}
		}
	}
	if len(expanded) == len(tools)-len(selected) {
		return errors.New("No permitted MCP tools match the request")
	}
	fields["tools"] = expanded
	return nil
}

func mcpInferenceServerMatch(server MCPServer, selectors []string) bool {
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" || server.matches(selector) {
			return true
		}
	}
	return false
}

func mcpInferenceToolAllowed(allowed any, name, fullName string) bool {
	if allowed == nil {
		return true
	}
	if object, ok := allowed.(map[string]any); ok {
		allowed = object["tool_names"]
	}
	selectors, _ := allowed.([]any)
	for _, value := range selectors {
		if value == name || value == fullName {
			return true
		}
	}
	return false
}
