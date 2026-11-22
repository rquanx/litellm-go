package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) handleMCPContent(w http.ResponseWriter, r *http.Request, req mcpRPCRequest, access MCPAccess) {
	servers := s.currentMCPServers()
	switch req.Method {
	case "prompts/list", "resources/list", "resources/templates/list":
		field := "prompts"
		if req.Method == "resources/list" {
			field = "resources"
		} else if req.Method == "resources/templates/list" {
			field = "resourceTemplates"
		}
		items := make([]map[string]any, 0)
		for _, server := range servers {
			if access(server, "") {
				items = append(items, s.listMCPContent(r.Context(), s.mcpRequestServer(r, server), req.Method, field)...)
			}
		}
		writeMCPResult(w, req.ID, map[string]any{field: items})
	case "prompts/get":
		var params struct {
			Name string `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
			writeMCPError(w, http.StatusOK, req.ID, -32602, "A prompt name is required")
			return
		}
		var selected MCPServer
		for _, server := range servers {
			if strings.HasPrefix(params.Name, server.Name+"-") && len(server.Name) > len(selected.Name) {
				selected = server
			}
		}
		name := strings.TrimPrefix(params.Name, selected.Name+"-")
		if selected.Name == "" || name == "" || !access(selected, "") {
			writeMCPError(w, http.StatusOK, req.ID, -32602, "Prompt is unavailable")
			return
		}
		result, err := s.requestMCPContent(r.Context(), s.mcpRequestServer(r, selected), req.Method,
			map[string]any{"name": name, "arguments": params.Arguments})
		if err != nil {
			writeMCPError(w, http.StatusOK, req.ID, -32603, "MCP prompt request failed")
			return
		}
		writeMCPResult(w, req.ID, result)
	case "resources/read":
		var params struct { URI string `json:"uri"` }
		if json.Unmarshal(req.Params, &params) != nil || params.URI == "" {
			writeMCPError(w, http.StatusOK, req.ID, -32602, "A resource URI is required")
			return
		}
		for _, server := range servers {
			if !access(server, "") {
				continue
			}
			result, err := s.requestMCPContent(r.Context(), s.mcpRequestServer(r, server), req.Method, map[string]string{"uri": params.URI})
			if err == nil {
				writeMCPResult(w, req.ID, result)
				return
			}
		}
		writeMCPError(w, http.StatusOK, req.ID, -32602, "Resource is unavailable")
	}
}

func (s *Server) requestMCPContent(ctx context.Context, server MCPServer, method string, params any) (json.RawMessage, error) {
	session, release, err := s.protocolMCPSession(ctx, server)
	if err != nil {
		return nil, err
	}
	defer release()
	return session.rpcContext(ctx, method, params)
}

func (s *Server) listMCPContent(ctx context.Context, server MCPServer, method, field string) []map[string]any {
	session, release, err := s.protocolMCPSession(ctx, server)
	if err != nil {
		return nil
	}
	defer release()
	var items []map[string]any
	params := make(map[string]string)
	for page := 0; page < 32; page++ {
		result, err := session.rpcContext(ctx, method, params)
		if err != nil {
			break
		}
		var data map[string]json.RawMessage
		var entries []map[string]any
		if json.Unmarshal(result, &data) != nil || json.Unmarshal(data[field], &entries) != nil {
			break
		}
		for _, entry := range entries {
			if name, ok := entry["name"].(string); ok && name != "" {
				entry["name"] = server.Name + "-" + name
				items = append(items, entry)
			}
		}
		var next string
		if json.Unmarshal(data["nextCursor"], &next) != nil || next == "" || next == params["cursor"] {
			break
		}
		params["cursor"] = next
	}
	return items
}
