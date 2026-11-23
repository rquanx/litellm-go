package proxyconfig

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type ssoIdentity struct {
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Verified bool   `json:"email_verified"`
	TeamIDs  []string
}

type cliSSOFlow struct {
	expires          time.Time
	pollHash         string
	codeHash         string
	browserTokenHash string
	identity         ssoIdentity
	userID           string
	attempts         int
	verified         bool
}

const cliSSOTTL = 10 * time.Minute

var cliConfirmation = template.Must(template.New("cli-confirm").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Confirm CLI sign-in</title><main><h1>Confirm CLI sign-in</h1><form method="post" action="{{.Action}}"><input type="hidden" name="browser_complete_token" value="{{.Token}}"><label for="user_code">Code from the CLI</label><input id="user_code" name="user_code" autocomplete="one-time-code" required><button>Confirm</button></form></main></html>`))

func (s *uiSSOServer) startCLI(w http.ResponseWriter, _ *http.Request) {
	if !s.config.ready() {
		modelError(w, http.StatusServiceUnavailable, "SSO is unavailable")
		return
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not generate a verification code")
		return
	}
	code := strings.ToUpper(hex.EncodeToString(random[:]))
	pollSecret := oauth2.GenerateVerifier()
	id := "cli-" + oauth2.GenerateVerifier()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, flow := range s.cli {
		if !flow.expires.After(now) {
			delete(s.cli, id)
		}
	}
	if len(s.cli) >= 4096 {
		modelError(w, http.StatusTooManyRequests, "Too many pending CLI logins")
		return
	}
	s.cli[id] = &cliSSOFlow{expires: now.Add(cliSSOTTL), pollHash: keyDigest(pollSecret), codeHash: keyDigest(code)}
	w.Header().Set("Cache-Control", "no-store")
	modelJSON(w, http.StatusOK, map[string]any{"login_id": id, "poll_secret": pollSecret,
		"user_code": code[:4] + "-" + code[4:], "expires_in": int(cliSSOTTL.Seconds())})
}

func (s *uiSSOServer) renderCLIConfirmation(w http.ResponseWriter, id string, identity ssoIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow := s.cli[id]
	if flow == nil || !flow.expires.After(time.Now()) || flow.browserTokenHash != "" {
		modelError(w, http.StatusNotFound, "Unknown or completed CLI login")
		return
	}
	s.keys.mu.RLock()
	user, _, _ := s.resolveUserLocked(w, identity.Subject, identity.Email, identity.Verified)
	s.keys.mu.RUnlock()
	if user.ID == "" {
		return
	}
	token := oauth2.GenerateVerifier()
	flow.browserTokenHash = keyDigest(token)
	flow.identity, flow.userID = identity, user.ID
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'")
	_ = cliConfirmation.Execute(w, struct{ Action, Token string }{s.config.base.JoinPath("sso/cli/complete", id).Path, token})
}

func (s *uiSSOServer) completeCLI(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		modelError(w, http.StatusBadRequest, "Invalid verification form")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("login_id")
	flow := s.cli[id]
	if flow == nil || !flow.expires.After(time.Now()) || flow.browserTokenHash == "" {
		modelError(w, http.StatusNotFound, "Unknown or incomplete CLI login")
		return
	}
	supplied := keyDigest(r.PostForm.Get("browser_complete_token"))
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(flow.browserTokenHash)) != 1 {
		modelError(w, http.StatusBadRequest, "Invalid verification code")
		return
	}
	code := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(r.PostForm.Get("user_code")), "-", ""))
	if subtle.ConstantTimeCompare([]byte(keyDigest(code)), []byte(flow.codeHash)) != 1 {
		flow.attempts++
		if flow.attempts >= 5 {
			delete(s.cli, id)
		}
		modelError(w, http.StatusBadRequest, "Invalid verification code")
		return
	}
	flow.verified = true
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><title>CLI sign-in confirmed</title><p>Return to the CLI to finish signing in.</p></html>"))
}

func (s *uiSSOServer) pollCLI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("login_id")
	flow := s.cli[id]
	if flow == nil || !flow.expires.After(time.Now()) {
		modelError(w, http.StatusNotFound, "Unknown CLI login")
		return
	}
	supplied := keyDigest(r.Header.Get("X-Litellm-CLI-Poll-Secret"))
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(flow.pollHash)) != 1 {
		modelError(w, http.StatusForbidden, "Invalid CLI polling secret")
		return
	}
	if !flow.verified {
		modelJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}

	s.keys.mu.Lock()
	defer s.keys.mu.Unlock()
	identity := flow.identity
	user, bound, binding := s.resolveUserLocked(w, identity.Subject, identity.Email, identity.Verified)
	if user.ID == "" {
		return
	}
	if user.ID != flow.userID {
		modelError(w, http.StatusForbidden, "CLI login user binding changed")
		return
	}
	next, ok := s.bindSSOUser(w, user, bound, binding, identity)
	if !ok {
		return
	}
	teams := make([]string, 0)
	for id, team := range next.Teams {
		if team.Blocked {
			continue
		}
		for _, member := range team.Members {
			if member.UserID == user.ID && member.Role != "internal_user_viewer" {
				teams = append(teams, id)
				break
			}
		}
	}
	slices.Sort(teams)
	details := make([]map[string]string, 0, len(teams))
	for _, id := range teams {
		details = append(details, map[string]string{"team_id": id, "team_alias": next.Teams[id].Alias})
	}
	result := map[string]any{"status": "ready", "user_id": user.ID, "teams": teams, "team_details": details}
	selected := r.URL.Query().Get("team_id")
	if selected == "" {
		if len(teams) > 1 {
			result["requires_team_selection"] = true
			modelJSON(w, http.StatusOK, result)
			return
		}
		if len(teams) == 1 {
			selected = teams[0]
		}
	} else if !slices.Contains(teams, selected) {
		modelError(w, http.StatusForbidden, "The user does not belong to the selected team")
		return
	}

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	token := newKeyToken()
	next.Keys = maps.Clone(next.Keys)
	next.Keys[keyDigest(token)] = virtualKey{Alias: "cli-sso:" + user.ID, UserID: user.ID, TeamID: selected, Models: []string{},
		Metadata: map[string]any{"_minato_cli_session": true}, CreatedAt: &now, Expires: &expires}
	if !s.keys.apply(w, next) {
		return
	}
	delete(s.cli, id)
	result["key"], result["team_id"] = token, nil
	if selected != "" {
		result["team_id"] = selected
	}
	modelJSON(w, http.StatusOK, result)
}
