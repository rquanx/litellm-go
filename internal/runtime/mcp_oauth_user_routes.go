package runtime

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

func (s *Server) serveMCPUserCredentials(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/v1/mcp/")
	if path != r.URL.Path {
		path = "/mcp/" + path
	}
	list := path == "/mcp/user-credentials" && r.Method == http.MethodGet
	id, operation, valid := strings.Cut(strings.TrimPrefix(path, "/mcp/server/"), "/")
	if !list && !(strings.HasPrefix(path, "/mcp/server/") && validResourceSegment(id) && valid &&
		((operation == "oauth-user-credential/status" || operation == "user-credential/status") && r.Method == http.MethodGet ||
			(operation == "oauth-user-credential" || operation == "user-credential") && (r.Method == http.MethodPost || r.Method == http.MethodDelete))) {
		return false
	}
	access, ok := s.mcpAccess(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "MCP server access is required")
		return true
	}
	if s.config.MCPUserOAuthStore == nil {
		writeError(w, http.StatusServiceUnavailable, "resource_error", "MCP OAuth credential storage is unavailable")
		return true
	}
	user := s.mcpOAuthUser(r)
	if user == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "An authenticated user is required")
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	store := s.config.MCPUserOAuthStore
	if list {
		records, err := store.List(r.Context(), user)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not list MCP OAuth credentials")
			return true
		}
		items := []map[string]any{}
		for _, server := range s.currentMCPServers() {
			if token, found := records[server.ID]; found && token.AccessToken != "" && access(server, "") {
				item := mcpUserTokenStatus(server.ID, token)
				item["server_name"] = server.Name
				item["credential_type"] = "oauth2"
				item["alias"] = server.Alias
				items = append(items, item)
			}
		}
		writeJSON(w, http.StatusOK, items)
		return true
	}
	for _, server := range s.currentMCPServers() {
		if server.matches(id) && access(server, "") && server.InteractiveOAuth != nil {
			s.manageMCPUserCredential(w, r, store, user, server.ID)
			return true
		}
	}
	writeError(w, http.StatusNotFound, "server_not_found", "MCP OAuth server not found")
	return true
}

func (s *Server) manageMCPUserCredential(w http.ResponseWriter, r *http.Request, store MCPUserOAuthStore, user, id string) {
	var token MCPUserOAuthToken
	var err error
	switch r.Method {
	case http.MethodPost:
		var input struct {
			Access  string   `json:"access_token"`
			Refresh string   `json:"refresh_token"`
			Expires int64    `json:"expires_in"`
			Scopes  []string `json:"scopes"`
		}
		if decodeBody(r, &input, 64<<10) != nil || strings.TrimSpace(input.Access) == "" || input.Expires < 0 ||
			input.Expires >= int64((1<<63-1)/time.Second) || strings.ContainsAny(input.Access, "\r\n") {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid MCP OAuth credential")
			return
		}
		token = MCPUserOAuthToken{Type: "oauth2", AccessToken: input.Access, RefreshToken: input.Refresh, Scopes: input.Scopes, ConnectedAt: time.Now().UTC()}
		if input.Expires > 0 {
			token.ExpiresAt = token.ConnectedAt.Add(time.Duration(input.Expires) * time.Second)
		}
		err = store.Save(r.Context(), user, id, token)
	case http.MethodDelete:
		err = store.Delete(r.Context(), user, id)
	case http.MethodGet:
		token, err = store.Read(r.Context(), user, id)
	}
	if err != nil && !errors.Is(err, ErrMCPUserOAuthNotFound) {
		writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not manage MCP OAuth credential")
		return
	}
	writeJSON(w, http.StatusOK, mcpUserTokenStatus(id, token))
}

func mcpUserTokenStatus(id string, token MCPUserOAuthToken) map[string]any {
	var expires, connected any
	if !token.ExpiresAt.IsZero() {
		expires = token.ExpiresAt
	}
	if !token.ConnectedAt.IsZero() {
		connected = token.ConnectedAt
	}
	return map[string]any{"server_id": id, "has_credential": token.AccessToken != "",
		"expires_at": expires, "is_expired": !token.ExpiresAt.IsZero() && time.Now().After(token.ExpiresAt), "connected_at": connected}
}
