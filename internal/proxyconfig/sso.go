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
	cli    map[string]*cliSSOFlow
}

type uiSSOFlow struct {
	verifier string
	expires  time.Time
	cliID    string
}

func (registry *KeyRegistry) registerUISSO(mux *http.ServeMux) {
	server := &uiSSOServer{keys: registry, config: loadUISSOConfig(), flows: make(map[string]uiSSOFlow), cli: make(map[string]*cliSSOFlow)}
	mux.HandleFunc("GET /sso/key/generate", server.start)
	mux.HandleFunc("GET /sso/callback", server.callback)
	mux.HandleFunc("GET /sso/readiness", server.readiness)
	mux.HandleFunc("POST /sso/cli/start", server.startCLI)
	mux.HandleFunc("POST /sso/cli/complete/{login_id}", server.completeCLI)
	mux.HandleFunc("GET /sso/cli/poll/{login_id}", server.pollCLI)
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
	var cliID string
	if r.URL.Query().Get("source") == "litellm-cli" {
		cliID = r.URL.Query().Get("key")
		flow := s.cli[cliID]
		if flow == nil || !flow.expires.After(now) || flow.browserTokenHash != "" {
			s.mu.Unlock()
			modelError(w, http.StatusNotFound, "Unknown or completed CLI login")
			return
		}
	}
	s.flows[state] = uiSSOFlow{verifier: verifier, expires: now.Add(uiSSOFlowTTL), cliID: cliID}
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
	var claims map[string]any
	subjectClaim, emailClaim := "sub", "email"
	if cfg.provider == "generic" {
		if field := os.Getenv("GENERIC_USER_ID_ATTRIBUTE"); field != "" {
			subjectClaim = field
		}
		if field := os.Getenv("GENERIC_USER_EMAIL_ATTRIBUTE"); field != "" {
			emailClaim = field
		}
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&claims) != nil {
		modelError(w, http.StatusUnauthorized, "SSO identity verification failed")
		return
	}
	var identity ssoIdentity
	identity.Subject, _ = ssoClaim(claims, subjectClaim).(string)
	identity.Email, _ = ssoClaim(claims, emailClaim).(string)
	identity.Verified, _ = claims["email_verified"].(bool)
	identity.TeamIDs = policyStringList(ssoClaim(claims, s.keys.ssoTeamClaim))
	identity.Role = cfg.userRole(claims)
	if identity.Subject == "" {
		modelError(w, http.StatusUnauthorized, "SSO identity verification failed")
		return
	}
	identity.Email = strings.TrimSpace(identity.Email)
	if flow.cliID != "" {
		s.renderCLIConfirmation(w, flow.cliID, identity)
		return
	}
	session, expires, ok := s.issueSession(w, identity)
	if !ok {
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "token", Value: session, Path: cfg.base.JoinPath(".").Path,
		Expires: expires, Secure: cfg.base.Scheme == "https", SameSite: http.SameSiteLaxMode})
	dashboard := cfg.base.JoinPath("ui")
	dashboard.RawQuery = "login=success"
	http.Redirect(w, r, dashboard.String(), http.StatusSeeOther)
}

// resolveUserLocked matches a provider identity to a preprovisioned user.
func (s *uiSSOServer) resolveUserLocked(w http.ResponseWriter, identity ssoIdentity) (internalUser, bool, string) {
	subject, email, verified := identity.Subject, identity.Email, identity.Verified
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
				return internalUser{}, false, ""
			}
			user, bound = candidate, true
		}
		if !bound && id == "" && verified && email != "" && strings.EqualFold(candidate.Email, email) {
			user = candidate
		}
	}
	if user.ID == "" && verified && email != "" && s.keys.ssoDefaults.enabled() {
		for _, candidate := range s.keys.state.Users {
			if strings.EqualFold(candidate.Email, email) {
				modelError(w, http.StatusForbidden, "SSO email is already bound to another identity")
				return internalUser{}, false, ""
			}
		}
		user = internalUser{ID: "sso-" + keyDigest(binding+"\x00"+subject), Role: s.keys.ssoDefaults.Role,
			Email: strings.ToLower(email), Models: s.keys.ssoDefaults.Models}
	}
	if user.ID == "" || !internalSSORole(user.Role) {
		modelError(w, http.StatusForbidden, "SSO identity has no active internal user")
		return internalUser{}, false, ""
	}
	if internalSSORole(identity.Role) {
		user.Role = identity.Role
	}
	return user, bound, binding
}

func (s *uiSSOServer) issueSession(w http.ResponseWriter, identity ssoIdentity) (string, time.Time, bool) {
	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	user, bound, binding := s.resolveUserLocked(w, identity)
	if user.ID == "" {
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

	next, ok := s.bindSSOUser(w, user, bound, binding, identity)
	if !ok {
		return "", time.Time{}, false
	}
	next.Keys = maps.Clone(next.Keys)
	for digest, key := range next.Keys {
		if key.Alias == "ui-sso:"+user.ID && key.UserID == user.ID && key.Expires != nil && !key.Expires.After(now) {
			delete(next.Keys, digest)
		}
	}
	key := virtualKey{Alias: "ui-sso:" + user.ID, UserID: user.ID, Models: []string{}, CreatedAt: &now, Expires: &expires}
	if user.Role == "internal_user_viewer" {
		key.Metadata = map[string]any{"_minato_sso_view_only": true}
	}
	next.Keys[keyDigest(token)] = key
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
	if _, ok := s.keys.identifyResourceCaller(r); ok {
		return true
	}
	modelError(w, http.StatusUnauthorized, "Proxy token required")
	return false
}

func (keys *KeyRegistry) ssoViewerLocked(r *http.Request, token string) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	switch r.URL.Path {
	case "/user/info", "/team/list", "/team/info", "/sso/readiness", "/sso/get/ui_settings":
	default:
		return "", false
	}
	key, ok := keys.state.Keys[keyDigest(token)]
	if !ok || key.Blocked || key.RotationTarget != "" || key.Expires == nil || !time.Now().Before(*key.Expires) {
		return "", false
	}
	readOnly, _ := key.Metadata["_minato_sso_view_only"].(bool)
	user := keys.state.Users[key.UserID]
	return user.ID, readOnly && user.Role == "internal_user_viewer" && key.Alias == "ui-sso:"+user.ID
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
