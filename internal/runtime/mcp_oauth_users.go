package runtime

import (
	"context"
	"net/http"
	"time"
)

type MCPUserOAuthToken struct {
	Type         string    `json:"type"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	ConnectedAt  time.Time `json:"connected_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}

type MCPUserOAuthStore interface {
	Read(context.Context, string, string) (MCPUserOAuthToken, error)
	Save(context.Context, string, string, MCPUserOAuthToken) error
}

func (s *Server) mcpOAuthUser(r *http.Request) string {
	if s.config.MCPUserOAuthIdentity == nil || s.config.MCPUserOAuthStore == nil {
		return ""
	}
	return s.config.MCPUserOAuthIdentity(r)
}

func (s *Server) loadMCPUserOAuth(r *http.Request, server MCPServer) string {
	if server.InteractiveOAuth == nil {
		return ""
	}
	if user := s.mcpOAuthUser(r); user != "" {
		token, err := s.config.MCPUserOAuthStore.Read(r.Context(), user, server.ID)
		if err == nil && (token.ExpiresAt.IsZero() || time.Now().Before(token.ExpiresAt)) {
			return token.AccessToken
		}
	}
	return ""
}
