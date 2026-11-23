package proxyconfig

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rquanx/minato-runtime/internal/runtime"
	"golang.org/x/oauth2"
)

type uiSSOServer struct {
	keys   *KeyRegistry
	config uiSSOConfig
	mu     sync.Mutex
	flows  map[string]uiSSOFlow
}

type uiSSOFlow struct {
	verifier string
	expires  time.Time
}

func (registry *KeyRegistry) registerUISSO(mux *http.ServeMux) {
	server := &uiSSOServer{keys: registry, config: loadUISSOConfig(), flows: make(map[string]uiSSOFlow)}
	mux.HandleFunc("GET /sso/key/generate", server.start)
	mux.HandleFunc("GET /sso/callback", server.callback)
	mux.HandleFunc("GET /sso/readiness", server.readiness)
	mux.HandleFunc("GET /sso/get/ui_settings", server.settings)
	if server.config.ready() {
		for _, path := range []string{"/.well-known/litellm-ui-config", "/litellm/.well-known/litellm-ui-config"} {
			mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
				writePublicUIConfig(w, true)
			})
		}
	}
}

func (s *uiSSOServer) start(w http.ResponseWriter, r *http.Request) {
	cfg := s.config
	if !cfg.ready() {
		modelError(w, http.StatusServiceUnavailable, "SSO is unavailable")
		return
	}
	now := time.Now()
	state := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()
	s.mu.Lock()
	for id, flow := range s.flows {
		if !flow.expires.After(now) {
			delete(s.flows, id)
		}
	}
	if len(s.flows) >= 4096 {
		s.mu.Unlock()
		modelError(w, http.StatusServiceUnavailable, "Too many pending SSO attempts")
		return
	}
	s.flows[state] = uiSSOFlow{verifier: verifier, expires: now.Add(uiSSOFlowTTL)}
	s.mu.Unlock()

	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, s.stateCookie(state, int(uiSSOFlowTTL.Seconds())))
	http.Redirect(w, r, cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *uiSSOServer) stateCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "litellm_sso_state", Value: value, Path: s.config.base.JoinPath("sso").Path,
		MaxAge: maxAge, HttpOnly: true, Secure: s.config.base.Scheme == "https", SameSite: http.SameSiteLaxMode}
}

func (s *uiSSOServer) callback(w http.ResponseWriter, r *http.Request) {
	cfg := s.config
	if !cfg.ready() {
		modelError(w, http.StatusServiceUnavailable, "SSO is unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	cookie, err := r.Cookie("litellm_sso_state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		modelError(w, http.StatusUnauthorized, "Invalid SSO state")
		return
	}
	s.mu.Lock()
	flow, exists := s.flows[state]
	delete(s.flows, state)
	s.mu.Unlock()
	http.SetCookie(w, s.stateCookie("", -1))
	if !exists || !flow.expires.After(time.Now()) || code == "" || query.Has("error") {
		modelError(w, http.StatusUnauthorized, "Invalid or expired SSO callback")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(flow.verifier))
	if err != nil || !token.Valid() {
		modelError(w, http.StatusUnauthorized, "SSO token exchange failed")
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.userInfo, nil)
	if err != nil {
		modelError(w, http.StatusBadGateway, "SSO user information request failed")
		return
	}
	request.Header.Set("Accept", "application/json")
	token.SetAuthHeader(request)
	response, err := client.Do(request)
	if err != nil {
		modelError(w, http.StatusBadGateway, "SSO user information request failed")
		return
	}
	defer response.Body.Close()
	var identity struct {
		Subject  string `json:"sub"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&identity) != nil || identity.Subject == "" {
		modelError(w, http.StatusUnauthorized, "SSO identity verification failed")
		return
	}
	session, expires, ok := s.issueSession(w, identity.Subject, strings.TrimSpace(identity.Email), identity.Verified)
	if !ok {
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "token", Value: session, Path: cfg.base.JoinPath(".").Path,
		Expires: expires, Secure: cfg.base.Scheme == "https", SameSite: http.SameSiteLaxMode})
	dashboard := cfg.base.JoinPath("ui")
	dashboard.RawQuery = "login=success"
	http.Redirect(w, r, dashboard.String(), http.StatusSeeOther)
}

func (s *uiSSOServer) issueSession(w http.ResponseWriter, subject, email string, verified bool) (string, time.Time, bool) {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	var user internalUser
	var bound bool
	binding := s.config.provider + "|" + s.config.ClientID + "|" + s.config.userInfo
	// The binding can be configured explicitly for providers that omit email_verified.
	for _, candidate := range s.keys.state.Users {
		identities, _ := candidate.Metadata["sso_subjects"].(map[string]any)
		id, _ := identities[binding].(string)
		if id == subject {
			if bound {
				modelError(w, http.StatusForbidden, "SSO subject has ambiguous user bindings")
				return "", time.Time{}, false
			}
			user, bound = candidate, true
		}
		if !bound && id == "" && verified && email != "" && strings.EqualFold(candidate.Email, email) {
			user = candidate
		}
	}
	if user.ID == "" || user.Role != "internal_user" {
		modelError(w, http.StatusForbidden, "SSO identity has no active internal user")
		return "", time.Time{}, false
	}

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	token := newKeyToken()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID, "key": token, "user_email": user.Email, "user_role": user.Role,
		"login_method": "sso", "premium_user": false, "auth_header_name": "Authorization",
		"disabled_non_admin_personal_key_creation": true, "server_root_path": os.Getenv("SERVER_ROOT_PATH"),
		"iat": now.Unix(), "exp": expires.Unix(),
	}).SignedString([]byte(s.keys.master))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not create SSO session")
		return "", time.Time{}, false
	}

	next := s.keys.state
	if !bound {
		metadata := maps.Clone(user.Metadata)
		if metadata == nil {
			metadata = make(map[string]any)
		}
		identities, _ := metadata["sso_subjects"].(map[string]any)
		identities = maps.Clone(identities)
		if identities == nil {
			identities = make(map[string]any)
		}
		identities[binding] = subject
		metadata["sso_subjects"] = identities
		user.Metadata = metadata
		next.Users = maps.Clone(next.Users)
		next.Users[user.ID] = user
	}
	next.Keys = maps.Clone(next.Keys)
	for digest, key := range next.Keys {
		if key.Alias == "ui-sso:"+user.ID && key.UserID == user.ID && key.Expires != nil && !key.Expires.After(now) {
			delete(next.Keys, digest)
		}
	}
	next.Keys[keyDigest(token)] = virtualKey{Alias: "ui-sso:" + user.ID, UserID: user.ID, Models: []string{}, CreatedAt: &now, Expires: &expires}
	if !s.keys.apply(w, next) {
		return "", time.Time{}, false
	}
	return signed, expires, true
}

func (s *uiSSOServer) authorized(w http.ResponseWriter, r *http.Request) bool {
	token := runtime.ProxyCredential(r)
	if subtle.ConstantTimeCompare([]byte(s.keys.master), []byte(token)) == 1 {
		return true
	}
	if _, ok := s.keys.Authorize(token); ok {
		return true
	}
	modelError(w, http.StatusUnauthorized, "Proxy token required")
	return false
}

func (s *uiSSOServer) readiness(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	cfg := s.config
	if cfg.provider == "" {
		modelJSON(w, http.StatusOK, map[string]any{"status": "healthy", "sso_configured": false, "message": "No SSO provider configured"})
		return
	}
	status := "healthy"
	if !cfg.ready() {
		status = "unhealthy"
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": status, "sso_configured": true, "provider": cfg.provider,
		"missing_env_vars": append([]string{}, cfg.missing...)})
}

func (s *uiSSOServer) settings(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	settings := map[string]any{"DEFAULT_TEAM_DISABLED": strings.EqualFold(os.Getenv("PROXY_DEFAULT_TEAM_DISABLED"), "true"),
		"SSO_ENABLED": s.config.ready(), "NUM_SPEND_LOGS_ROWS": 0, "DISABLE_EXPENSIVE_DB_QUERIES": false}
	for _, field := range []string{"PROXY_BASE_URL", "PROXY_LOGOUT_URL", "LITELLM_UI_API_DOC_BASE_URL"} {
		settings[field] = nil
		if value, ok := os.LookupEnv(field); ok {
			settings[field] = value
		}
	}
	modelJSON(w, http.StatusOK, settings)
}
