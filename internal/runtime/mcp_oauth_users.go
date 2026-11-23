package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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

var ErrMCPUserOAuthNotFound = errors.New("MCP OAuth credential not found")

type MCPUserOAuthStore interface {
	Read(context.Context, string, string) (MCPUserOAuthToken, error)
	Save(context.Context, string, string, MCPUserOAuthToken) error
	Delete(context.Context, string, string) error
	List(context.Context, string) (map[string]MCPUserOAuthToken, error)
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
	user := s.mcpOAuthUser(r)
	if user == "" {
		return ""
	}
	store := s.config.MCPUserOAuthStore
	token, err := store.Read(r.Context(), user, server.ID)
	if err != nil {
		return ""
	}
	if token.ExpiresAt.IsZero() || time.Now().Before(token.ExpiresAt) {
		return token.AccessToken
	}
	if token.RefreshToken == "" || server.InteractiveOAuth.ClientID == "" {
		return ""
	}
	type refreshKey struct{ user, server string }
	key := refreshKey{user, server.ID}
	done := make(chan struct{})
	if running, loaded := s.mcpOAuthRefresh.LoadOrStore(key, done); loaded {
		select {
		case <-running.(chan struct{}):
			token, err = store.Read(r.Context(), user, server.ID)
			if err == nil && (token.ExpiresAt.IsZero() || time.Now().Before(token.ExpiresAt)) {
				return token.AccessToken
			}
		case <-r.Context().Done():
		}
		return ""
	}
	defer func() { s.mcpOAuthRefresh.Delete(key); close(done) }()
	token, err = store.Read(r.Context(), user, server.ID)
	if err != nil || token.RefreshToken == "" {
		return ""
	}
	if token.ExpiresAt.IsZero() || time.Now().Before(token.ExpiresAt) {
		return token.AccessToken
	}
	oauth := server.InteractiveOAuth
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken}, "client_id": {oauth.ClientID}}
	if oauth.ClientSecret != "" {
		form.Set("client_secret", oauth.ClientSecret)
	}
	refreshed, _, err := s.fetchMCPOAuthToken(r.Context(), oauth.TokenURL, form)
	if err != nil {
		return ""
	}
	if len(refreshed.Scopes) == 0 {
		refreshed.Scopes = token.Scopes
	}
	if err := store.Save(r.Context(), user, server.ID, refreshed); err != nil {
		return ""
	}
	return refreshed.AccessToken
}
