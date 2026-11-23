package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type MCPInteractiveOAuth struct {
	AuthorizationURL, TokenURL, CallbackURL, RegistrationURL, ClientID, ClientSecret string
	Scopes                                                                           []string
}

func (oauth *MCPInteractiveOAuth) Validate() error {
	addresses := []string{oauth.AuthorizationURL, oauth.TokenURL, oauth.CallbackURL}
	if oauth.RegistrationURL != "" {
		addresses = append(addresses, oauth.RegistrationURL)
	}
	for _, value := range addresses {
		address, err := url.Parse(value)
		if err != nil || address.Hostname() == "" || address.User != nil || address.Fragment != "" ||
			(address.Scheme != "https" && !(address.Scheme == "http" && mcpOAuthLoopback(address.Hostname()))) {
			return errors.New("interactive MCP OAuth requires trusted authorization, token and callback URLs")
		}
	}
	callback, _ := url.Parse(oauth.CallbackURL)
	if callback.Path != "/callback" || callback.RawQuery != "" {
		return errors.New("the MCP OAuth callback URL must end in /callback")
	}
	return nil
}

func mcpOAuthLoopback(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

type mcpOAuthFlow struct {
	serverID, clientID, redirect, originalState, challenge, state string
	owner                                                         [32]byte
	fingerprint                                                   [32]byte
	expires                                                       time.Time
}

type mcpOAuthFlows struct {
	mu     sync.Mutex
	states map[string]mcpOAuthFlow
	codes  map[[32]byte]mcpOAuthFlow
}

func (oauth *MCPInteractiveOAuth) fingerprint() [32]byte {
	data, _ := json.Marshal(oauth)
	return sha256.Sum256(data)
}

func (s *Server) serveMCPOAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.serveMCPOAuthDiscovery(w, r) {
		return true
	}
	path := strings.Trim(r.URL.Path, "/")
	if path == "callback" && r.Method == http.MethodGet {
		if !strings.HasPrefix(r.URL.Query().Get("state"), "minato-mcp-") {
			return false
		}
		s.completeMCPOAuth(w, r)
		return true
	}
	selector, action, named := strings.Cut(path, "/")
	if !named {
		selector, action = "", selector
	}
	if (action != "authorize" || r.Method != http.MethodGet) && ((action != "token" && action != "register") || r.Method != http.MethodPost) {
		return false
	}
	var server MCPServer
	matches := 0
	for _, candidate := range s.currentMCPServers() {
		if candidate.InteractiveOAuth != nil && (selector == "" || candidate.matches(selector)) {
			server = candidate
			matches++
		}
	}
	if matches != 1 {
		return false
	}
	access, ok := s.mcpAccess(r)
	if !ok || !access(server, "") {
		writeError(w, http.StatusUnauthorized, "unauthorized", "MCP server access is required")
		return true
	}
	switch action {
	case "authorize":
		s.startMCPOAuth(w, r, server)
	case "register":
		s.registerMCPOAuthClient(w, r, server)
	default:
		s.exchangeMCPOAuth(w, r, server)
	}
	return true
}

func (s *Server) startMCPOAuth(w http.ResponseWriter, r *http.Request, server MCPServer) {
	oauth := server.InteractiveOAuth
	options := r.URL.Query()
	client := oauth.ClientID
	if client == "" {
		client = options.Get("client_id")
	}
	redirect := options.Get("redirect_uri")
	target, err := url.Parse(redirect)
	own, _ := url.Parse(oauth.CallbackURL)
	challenge := options.Get("code_challenge")
	digest, pkceErr := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || target.User != nil || target.Fragment != "" || target.Hostname() == "" ||
		!((target.Scheme == "http" || target.Scheme == "https") && (mcpOAuthLoopback(target.Hostname()) || sameMCPOrigin(target, own))) ||
		client == "" || pkceErr != nil || len(digest) != sha256.Size || options.Get("code_challenge_method") != "S256" ||
		(options.Get("response_type") != "" && options.Get("response_type") != "code") {
		writeError(w, http.StatusBadRequest, "invalid_request", "MCP OAuth requires an allowed redirect and S256 PKCE challenge")
		return
	}
	flow := mcpOAuthFlow{serverID: server.ID, clientID: client, owner: sha256.Sum256([]byte(ProxyCredential(r))), redirect: redirect, originalState: options.Get("state"),
		challenge: challenge, state: "minato-mcp-" + newUUID() + newUUID(), fingerprint: oauth.fingerprint(), expires: time.Now().Add(10 * time.Minute)}
	store := &s.mcpOAuthFlows
	store.mu.Lock()
	for key, value := range store.states {
		if time.Now().After(value.expires) {
			delete(store.states, key)
		}
	}
	for key, value := range store.codes {
		if time.Now().After(value.expires) {
			delete(store.codes, key)
		}
	}
	if len(store.states)+len(store.codes) >= 2048 {
		store.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "resource_error", "MCP OAuth authorization capacity reached")
		return
	}
	if store.states == nil {
		store.states = make(map[string]mcpOAuthFlow)
		store.codes = make(map[[32]byte]mcpOAuthFlow)
	}
	store.states[flow.state] = flow
	store.mu.Unlock()
	upstream, _ := url.Parse(oauth.AuthorizationURL)
	query := upstream.Query()
	query.Set("client_id", client)
	query.Set("redirect_uri", oauth.CallbackURL)
	query.Set("response_type", "code")
	query.Set("state", flow.state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	scope := options.Get("scope")
	if scope == "" {
		scope = strings.Join(oauth.Scopes, " ")
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	upstream.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, upstream.String(), http.StatusFound)
}

func (s *Server) completeMCPOAuth(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	store := &s.mcpOAuthFlows
	store.mu.Lock()
	flow, found := store.states[state]
	if found {
		delete(store.states, state)
	}
	codeKey := sha256.Sum256([]byte(code))
	_, duplicate := store.codes[codeKey]
	found = found && !duplicate && time.Now().Before(flow.expires) && code != ""
	if found {
		store.codes[codeKey] = flow
	}
	store.mu.Unlock()
	if !found {
		writeError(w, http.StatusBadRequest, "invalid_request", "Unknown or expired MCP OAuth authorization")
		return
	}
	destination, _ := url.Parse(flow.redirect)
	values := destination.Query()
	values.Set("code", code)
	values.Set("state", flow.originalState)
	destination.RawQuery = values.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, destination.String(), http.StatusFound)
}

func (s *Server) exchangeMCPOAuth(w http.ResponseWriter, r *http.Request, server MCPServer) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid MCP OAuth token request")
		return
	}
	oauth := server.InteractiveOAuth
	grant := r.PostForm.Get("grant_type")
	form := url.Values{"grant_type": {grant}}
	clientID := oauth.ClientID
	if clientID == "" {
		clientID = r.PostForm.Get("client_id")
	}
	switch grant {
	case "authorization_code":
		code, verifier := r.PostForm.Get("code"), r.PostForm.Get("code_verifier")
		challenge := sha256.Sum256([]byte(verifier))
		codeKey := sha256.Sum256([]byte(code))
		store := &s.mcpOAuthFlows
		store.mu.Lock()
		flow, found := store.codes[codeKey]
		valid := found && len(verifier) >= 43 && len(verifier) <= 128 && time.Now().Before(flow.expires) &&
			flow.serverID == server.ID && flow.owner == sha256.Sum256([]byte(ProxyCredential(r))) && flow.fingerprint == oauth.fingerprint() &&
			flow.clientID == clientID && flow.challenge == base64.RawURLEncoding.EncodeToString(challenge[:]) &&
			(r.PostForm.Get("redirect_uri") == "" || r.PostForm.Get("redirect_uri") == flow.redirect)
		if valid {
			delete(store.codes, codeKey)
		}
		store.mu.Unlock()
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid_grant", "Unknown, expired or mismatched MCP OAuth grant")
			return
		}
		form.Set("code", code)
		form.Set("code_verifier", verifier)
		form.Set("redirect_uri", oauth.CallbackURL)
	case "refresh_token":
		if refresh := r.PostForm.Get("refresh_token"); refresh != "" {
			form.Set("refresh_token", refresh)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "MCP OAuth refresh_token is required")
			return
		}
		if scope := r.PostForm.Get("scope"); scope != "" {
			form.Set("scope", scope)
		}
	default:
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "Unsupported MCP OAuth grant")
		return
	}
	if clientID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "MCP OAuth client_id is required")
		return
	}
	form.Set("client_id", clientID)
	secret := oauth.ClientSecret
	if secret == "" {
		secret = r.PostForm.Get("client_secret")
	}
	if secret != "" {
		form.Set("client_secret", secret)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid MCP OAuth provider configuration")
		return
	}
	upstream.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	upstream.Header.Set("Accept", "application/json")
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		writeError(w, http.StatusServiceUnavailable, "resource_error", "MCP OAuth token exchange timed out")
		return
	}
	defer func() { <-s.sem }()
	status, data, _, err := s.sendGeminiFileRequest(&client, upstream)
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err != nil || status/100 != 2 || json.Unmarshal(data, &token) != nil || token.AccessToken == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "MCP OAuth token exchange failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if user := s.mcpOAuthUser(r); user != "" {
		record := MCPUserOAuthToken{Type: "oauth2", AccessToken: token.AccessToken, RefreshToken: token.RefreshToken,
			Scopes: strings.Fields(token.Scope), ConnectedAt: time.Now().UTC()}
		if record.RefreshToken == "" && grant == "refresh_token" {
			record.RefreshToken = form.Get("refresh_token")
		}
		if token.ExpiresIn > 0 && token.ExpiresIn < int64((1<<63-1)/time.Second) {
			record.ExpiresAt = record.ConnectedAt.Add(time.Duration(token.ExpiresIn) * time.Second)
		}
		_ = s.config.MCPUserOAuthStore.Save(ctx, user, server.ID, record)
	}
	writeRawResponse(w, http.StatusOK, http.Header{"Content-Type": {"application/json"}}, data)
}
