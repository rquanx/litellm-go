package proxyconfig

import (
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type dashboardInvitation struct {
	ID string `json:"id"`
	UserID string `json:"user_id"`
	Accepted bool `json:"is_accepted"`
	AcceptedAt *time.Time `json:"accepted_at"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

func (keys *KeyRegistry) registerDashboardInvitationRoutes(mux *http.ServeMux) {
	if keys.master == "" {
		return
	}
	mux.HandleFunc("POST /invitation/new", keys.createDashboardInvitation)
	mux.HandleFunc("GET /invitation/info", keys.dashboardInvitationInfo)
	mux.HandleFunc("POST /invitation/delete", keys.deleteDashboardInvitation)
	mux.HandleFunc("POST /invitation/update", keys.updateDashboardInvitation)
	if !strings.EqualFold(os.Getenv("DISABLE_ADMIN_UI"), "true") {
		mux.HandleFunc("GET /onboarding/get_token", keys.dashboardOnboardingToken)
		mux.HandleFunc("POST /onboarding/claim_token", keys.claimDashboardInvitation)
	}
}

func (keys *KeyRegistry) updateDashboardInvitation(w http.ResponseWriter, r *http.Request) {
	caller, authorized := keys.identifyResourceCaller(r)
	if !authorized {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	var input struct {
		ID string `json:"invitation_id"`
		Accepted *bool `json:"is_accepted"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 16<<10), &input); err != nil ||
		input.ID == "" || input.Accepted == nil {
		modelError(w, http.StatusBadRequest, "Supply an invitation ID and acceptance state")
		return
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	invite := keys.state.Invitations[input.ID]
	if invite.ID == "" || (!caller.admin && caller.user != invite.UserID) {
		modelError(w, http.StatusBadRequest, "Invitation does not exist")
		return
	}
	now := time.Now().UTC()
	if invite.AcceptedAt == nil {
		invite.AcceptedAt = &now
	}
	invite.Accepted, invite.UpdatedAt, invite.UpdatedBy = *input.Accepted, now, caller.user
	if caller.admin {
		invite.UpdatedBy = "default_user_id"
	}
	next := keys.state
	next.Invitations = maps.Clone(next.Invitations)
	next.Invitations[invite.ID] = invite
	if keys.apply(w, next) {
		modelJSON(w, http.StatusOK, invite)
	}
}

func (keys *KeyRegistry) createDashboardInvitation(w http.ResponseWriter, r *http.Request) {
	if !keys.requireMaster(w, r) {
		return
	}
	var input struct { UserID string `json:"user_id"` }
	if err := decodeModelJSON(http.MaxBytesReader(w,r.Body,16<<10), &input); err != nil || input.UserID == "" {
		modelError(w, http.StatusBadRequest, "Supply a user ID")
		return
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	user := keys.state.Users[input.UserID]
	if user.ID == "" || user.Email == "" || !internalSSORole(user.Role) {
		modelError(w, http.StatusBadRequest, "User needs an email and an internal role")
		return
	}
	now := time.Now().UTC()
	invite := dashboardInvitation{ID: newEntityID(), UserID: user.ID, ExpiresAt: now.Add(7*24*time.Hour),
		CreatedAt: now, UpdatedAt: now, CreatedBy: "default_user_id", UpdatedBy: "default_user_id"}
	next := keys.state
	next.Invitations = make(map[string]dashboardInvitation, len(keys.state.Invitations)+1)
	maps.Copy(next.Invitations, keys.state.Invitations)
	next.Invitations[invite.ID] = invite
	if keys.apply(w, next) {
		modelJSON(w, http.StatusOK, invite)
	}
}

func (keys *KeyRegistry) dashboardInvitationInfo(w http.ResponseWriter, r *http.Request) {
	if !keys.requireMaster(w, r) {
		return
	}
	keys.mu.RLock()
	invite, exists := keys.state.Invitations[r.URL.Query().Get("invitation_id")]
	keys.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusBadRequest, "Invitation does not exist")
		return
	}
	modelJSON(w, http.StatusOK, invite)
}

func (keys *KeyRegistry) deleteDashboardInvitation(w http.ResponseWriter, r *http.Request) {
	if !keys.requireMaster(w, r) {
		return
	}
	var input struct { ID string `json:"invitation_id"` }
	if err := decodeModelJSON(http.MaxBytesReader(w,r.Body,16<<10), &input); err != nil || input.ID == "" {
		modelError(w, http.StatusBadRequest, "Supply an invitation ID")
		return
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	invite, exists := keys.state.Invitations[input.ID]
	if !exists {
		modelError(w, http.StatusBadRequest, "Invitation does not exist")
		return
	}
	next := keys.state
	next.Invitations = maps.Clone(next.Invitations)
	delete(next.Invitations, input.ID)
	if keys.apply(w, next) {
		modelJSON(w, http.StatusOK, invite)
	}
}

func (keys *KeyRegistry) dashboardOnboardingToken(w http.ResponseWriter, r *http.Request) {
	keys.mu.RLock()
	invite := keys.state.Invitations[r.URL.Query().Get("invite_link")]
	user := keys.state.Users[invite.UserID]
	keys.mu.RUnlock()
	if !invite.claimable(user, time.Now()) {
		modelError(w, http.StatusUnauthorized, "Invalid or expired invitation")
		return
	}
	now := time.Now().UTC()
	claims := jwt.MapClaims{"invitation_link": invite.ID, "user_id": user.ID, "token_type": "litellm_onboarding",
		"exp": min(now.Add(15*time.Minute).Unix(), invite.ExpiresAt.Unix())}
	onboarding, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(keys.master))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not issue onboarding token")
		return
	}
	wrapper := jwt.MapClaims{"key": onboarding, "user_id": user.ID, "user_email": user.Email, "user_role": user.Role,
		"login_method": "username_password", "auth_header_name": "Authorization",
		"premium_user": false, "server_root_path": os.Getenv("SERVER_ROOT_PATH"), "exp": claims["exp"]}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, wrapper).SignedString([]byte(keys.master))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not issue onboarding token")
		return
	}
	redirect := dashboardPath().JoinPath("ui", "onboarding")
	redirect.RawQuery = url.Values{"token": {signed}, "user_email": {user.Email}}.Encode()
	w.Header().Set("Cache-Control", "no-store")
	modelJSON(w, http.StatusOK, map[string]string{"login_url": redirect.String(), "token": signed, "user_email": user.Email})
}

func (keys *KeyRegistry) claimDashboardInvitation(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"invitation_link"`
		UserID string `json:"user_id"`
		Password string `json:"password"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w,r.Body,16<<10), &input); err != nil ||
		input.ID == "" || input.UserID == "" || input.Password == "" {
		modelError(w, http.StatusBadRequest, "Invalid onboarding request")
		return
	}
	credential := r.Header.Get("Authorization")
	if len(credential) > 7 && strings.EqualFold(credential[:7], "Bearer ") {
		credential = strings.TrimSpace(credential[7:])
	}
	if len(credential) > 8192 {
		modelError(w, http.StatusUnauthorized, "Invalid onboarding session")
		return
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(credential, claims, func(*jwt.Token) (any,error) { return []byte(keys.master),nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || claims["token_type"] != "litellm_onboarding" || claims["invitation_link"] != input.ID || claims["user_id"] != input.UserID {
		modelError(w, http.StatusUnauthorized, "Invalid onboarding session")
		return
	}
	keys.mu.RLock()
	invite := keys.state.Invitations[input.ID]
	user := keys.state.Users[input.UserID]
	keys.mu.RUnlock()
	if invite.UserID != input.UserID || !invite.claimable(user, time.Now()) {
		modelError(w, http.StatusUnauthorized, "Invalid or expired invitation")
		return
	}
	select {
	case keys.passwordSlots <- struct{}{}:
		defer func() { <-keys.passwordSlots }()
	default:
		modelError(w, http.StatusTooManyRequests, "Too many concurrent password checks")
		return
	}
	hash, err := hashDashboardPassword(input.Password)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not set user password")
		return
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	now := time.Now().UTC()
	invite = keys.state.Invitations[input.ID]
	user = keys.state.Users[input.UserID]
	expiry, _ := claims.GetExpirationTime()
	if expiry == nil || !now.Before(expiry.Time) || invite.UserID != input.UserID || !invite.claimable(user, now) {
		modelError(w, http.StatusUnauthorized, "Invalid or expired invitation")
		return
	}
	next := keys.state
	next.setDashboardPassword(user.ID, hash)
	next.Invitations = maps.Clone(next.Invitations)
	invite.Accepted, invite.AcceptedAt, invite.UpdatedAt, invite.UpdatedBy = true, &now, now, user.ID
	next.Invitations[invite.ID] = invite
	session, _, ok := keys.issueUserDashboardSession(w, next, user, "ui-password:", "username_password")
	if !ok {
		return
	}
	redirect := dashboardPath().JoinPath("ui")
	redirect.Path += "/"
	redirect.RawQuery = "login=success"
	w.Header().Set("Cache-Control", "no-store")
	user.ImportedSpend = nil
	modelJSON(w, http.StatusOK, map[string]any{"login_url": redirect.String(), "token": session, "user_email": user.Email, "user": user})
}

func (invite dashboardInvitation) claimable(user internalUser, now time.Time) bool {
	return invite.ID != "" && !invite.Accepted && invite.AcceptedAt == nil && now.Before(invite.ExpiresAt) &&
		user.ID != "" && invite.UserID == user.ID && user.Email != "" && internalSSORole(user.Role)
}
