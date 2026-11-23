package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) registerMCPOAuthClient(w http.ResponseWriter, r *http.Request, server MCPServer) {
	oauth := server.InteractiveOAuth
	var input struct {
		Name       string   `json:"client_name"`
		Grants     []string `json:"grant_types"`
		Responses  []string `json:"response_types"`
		AuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := decodeBody(r, &input, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid MCP OAuth registration request")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if (oauth.ClientID != "" && oauth.ClientSecret != "") || oauth.RegistrationURL == "" {
		id := oauth.ClientID
		if id == "" {
			id = server.Name
		}
		writeJSON(w, http.StatusOK, map[string]any{"client_id": id, "client_secret": "dummy", "redirect_uris": []string{oauth.CallbackURL}})
		return
	}
	if input.Grants == nil {
		input.Grants = []string{}
	}
	if input.Responses == nil {
		input.Responses = []string{}
	}
	body, _ := json.Marshal(map[string]any{
		"client_name": input.Name, "grant_types": input.Grants, "response_types": input.Responses,
		"redirect_uris": []string{oauth.CallbackURL}, "token_endpoint_auth_method": input.AuthMethod,
	})
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, oauth.RegistrationURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid MCP OAuth registration endpoint")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, "resource_error", "MCP OAuth registration timed out")
		return
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, data, _, err := s.sendGeminiFileRequest(&client, upstream)
	var result struct {
		ClientID string `json:"client_id"`
	}
	if err != nil || status/100 != 2 || json.Unmarshal(data, &result) != nil || result.ClientID == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "MCP OAuth client registration failed")
		return
	}
	writeRawResponse(w, status, http.Header{"Content-Type": {"application/json"}}, data)
}
