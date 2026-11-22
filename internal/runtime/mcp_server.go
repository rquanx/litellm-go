package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
)

type mcpRPCRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	admin := s.authorized(r)
	if !admin {
		valid := false
		if s.config.AuthorizeKey != nil {
			_, valid = s.config.AuthorizeKey(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req mcpRPCRequest
	body, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if json.Unmarshal(body, &req) != nil {
		writeMCPError(w, http.StatusBadRequest, nil, -32700, "Parse error")
		return
	}
	if req.Version != "2.0" || req.Method == "" {
		writeMCPError(w, http.StatusBadRequest, req.ID, -32600, "Invalid request")
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct { ProtocolVersion string `json:"protocolVersion"` }
		_ = json.Unmarshal(req.Params, &params)
		switch params.ProtocolVersion {
		case "2025-11-25", "2025-06-18", "2025-03-26":
		default:
			params.ProtocolVersion = "2025-03-26"
		}
		writeMCPResult(w, req.ID, map[string]any{"protocolVersion": params.ProtocolVersion,
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]string{"name": "minato-runtime", "version": "1"}})
	case "ping":
		writeMCPResult(w, req.ID, map[string]any{})
	case "tools/list", "tools/call":
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-r.Context().Done():
			return
		}
		if req.Method == "tools/list" {
			s.handleMCPList(w, r, req, admin)
		} else {
			s.handleMCPCall(w, r, req, admin)
		}
	default:
		writeMCPError(w, http.StatusOK, req.ID, -32601, "Method not found")
	}
}

func (s *Server) handleMCPList(w http.ResponseWriter, r *http.Request, req mcpRPCRequest, admin bool) {
	tools := make([]map[string]any, 0)
	for _, server := range s.config.MCPServers {
		if !admin && !server.AllowAllKeys {
			continue
		}
		entries, err := s.mcpTools(r.Context(), server)
		if err != nil {
			continue
		}
		for _, tool := range entries {
			tool["name"] = server.Name + "-" + tool["name"].(string)
			delete(tool, "mcp_info")
			tools = append(tools, tool)
		}
	}
	writeMCPResult(w, req.ID, map[string]any{"tools": tools})
}

func (s *Server) handleMCPCall(w http.ResponseWriter, r *http.Request, req mcpRPCRequest, admin bool) {
	var params struct {
		Name string `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
		writeMCPError(w, http.StatusOK, req.ID, -32602, "Invalid tool arguments")
		return
	}
	var selected MCPServer
	for _, server := range s.config.MCPServers {
		if strings.HasPrefix(params.Name, server.Name+"-") && len(server.Name) > len(selected.Name) {
			selected = server
		}
	}
	name := strings.TrimPrefix(params.Name, selected.Name+"-")
	if selected.Name == "" || (!admin && !selected.AllowAllKeys) || !selected.allowsTool(name) {
		writeMCPError(w, http.StatusOK, req.ID, -32602, "Tool is unavailable")
		return
	}
	session, err := s.openMCP(r.Context(), selected)
	if err != nil {
		writeMCPError(w, http.StatusOK, req.ID, -32603, "Could not open MCP session")
		return
	}
	defer session.close()
	if params.Arguments == nil {
		params.Arguments = make(map[string]json.RawMessage)
	}
	response, err := session.rpc("tools/call", map[string]any{"name": name, "arguments": params.Arguments})
	if err != nil {
		writeMCPError(w, http.StatusOK, req.ID, -32603, "MCP tool call failed")
		return
	}
	writeMCPResult(w, req.ID, response)
}

func writeMCPError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
