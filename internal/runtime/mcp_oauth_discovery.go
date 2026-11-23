package runtime

import (
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) serveMCPOAuthDiscovery(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/.well-known/"
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	kind, selector, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if kind != "oauth-protected-resource" && kind != "oauth-authorization-server" {
		return false
	}
	standard := strings.HasPrefix(selector, "mcp/")
	if standard {
		selector = strings.TrimPrefix(selector, "mcp/")
	} else {
		selector = strings.TrimSuffix(selector, "/mcp")
	}
	if selector == "mcp" {
		selector = ""
	}
	var oauth *MCPInteractiveOAuth
	name := selector
	for _, server := range s.currentMCPServers() {
		if server.InteractiveOAuth == nil || (selector != "" && !server.matches(selector)) {
			continue
		}
		if oauth != nil {
			return false
		}
		oauth = server.InteractiveOAuth
		if selector == "" {
			name = server.Name
		}
	}
	if oauth == nil || !validResourceSegment(name) {
		return false
	}
	base := strings.TrimSuffix(oauth.CallbackURL, "/callback")
	issuer := base + "/" + url.PathEscape(name)
	scopes := oauth.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	if kind == "oauth-authorization-server" {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"response_types_supported": []string{"code"}, "scopes_supported": scopes,
			"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		})
		return true
	}
	resource := issuer + "/mcp"
	if standard {
		resource = base + "/mcp/" + url.PathEscape(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorization_servers": []string{issuer}, "resource": resource, "scopes_supported": scopes})
	return true
}
