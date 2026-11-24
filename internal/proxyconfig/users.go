package proxyconfig

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type internalUser struct {
	ownerRates
	DefaultOrgID string `json:"default_organization_id,omitempty"`
	ID       string         `json:"user_id"`
	Alias    string         `json:"user_alias"`
	Email    string         `json:"user_email"`
	Role     string         `json:"user_role"`
	Models   []string       `json:"models"`
	Metadata map[string]any `json:"metadata"`
	MaxBudget *float64 `json:"max_budget"`
	BudgetDuration string `json:"budget_duration,omitempty"`
	BudgetResetAt *time.Time `json:"budget_reset_at"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
}

func (registry *KeyRegistry) registerUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /user/new", func(w http.ResponseWriter, r *http.Request) {
		registry.writeUser(w, r, false)
	})
	mux.HandleFunc("POST /user/update", func(w http.ResponseWriter, r *http.Request) {
		registry.writeUser(w, r, true)
	})
	mux.HandleFunc("GET /user/info", registry.userInfo)
	mux.HandleFunc("GET /v2/user/info", registry.userInfoV2)
	mux.HandleFunc("GET /user/available_roles", registry.availableUserRoles)
	mux.HandleFunc("GET /user/list", registry.listUsers)
	mux.HandleFunc("GET /user/available_users", func(w http.ResponseWriter, r *http.Request) {
		registry.searchDashboardUsers(w, r, true)
	})
	mux.HandleFunc("POST /user/delete", registry.deleteUsers)
	mux.HandleFunc("POST /user/bulk_update", registry.bulkUpdateUsers)
}

func (registry *KeyRegistry) availableUserRoles(w http.ResponseWriter, r *http.Request) {
	if _, valid := registry.identifyReadCaller(r); !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	type description struct {
		Text string `json:"description"`
		Label string `json:"ui_label"`
	}
	modelJSON(w, http.StatusOK, map[string]description{
		"proxy_admin": {Text: "admin over litellm proxy, has all permissions", Label: "Admin (All Permissions)"},
		"proxy_admin_viewer": {Text: "view all keys, view all spend", Label: "Admin (View Only)"},
		"internal_user": {Text: "view/create/delete their own keys, view their own spend", Label: "Internal User (Create/Delete/View)"},
		"internal_user_viewer": {Text: "view their own keys, view their own spend", Label: "Internal User (View Only)"},
	})
}

func (registry *KeyRegistry) userInfoV2(w http.ResponseWriter, r *http.Request) {
	caller, valid := registry.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	id := r.URL.Query().Get("user_id")
	if id == "" {
		id = caller.user
	}
	if id == "" {
		modelError(w, http.StatusBadRequest, "Supply a user ID")
		return
	}
	registry.mu.RLock()
	user, exists := registry.state.Users[id]
	if caller.admin && id == "default_user_id" && !exists {
		user, exists = internalUser{ID: id, Role: "proxy_admin"}, true
	}
	allowed := caller.admin || caller.user == id
	teams := []string{}
	if exists {
		for teamID, team := range registry.state.Teams {
			if memberIndex(team, id) < 0 {
				continue
			}
			teams = append(teams, teamID)
			if index := memberIndex(team, caller.user); !team.Blocked && caller.user != "" && index >= 0 && team.Members[index].Role == "admin" {
				allowed = true
			}
		}
	}
	registry.mu.RUnlock()
	if !exists || !allowed {
		modelError(w, http.StatusNotFound, "User not found")
		return
	}
	slices.Sort(teams)
	start, reset, spend := registry.spendWindow(user.BudgetDuration, user.BudgetResetAt, user.ImportedSpend, time.Now())
	user.BudgetResetAt = reset
	user.ImportedSpend = nil
	if registry.userSpend != nil {
		recorded, err := registry.userSpend(r.Context(), id, start)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read user spend")
			return
		}
		spend += recorded
	}
	if user.Models == nil {
		user.Models = []string{}
	}
	modelJSON(w, http.StatusOK, struct {
		internalUser
		Spend float64  `json:"spend"`
		Teams []string `json:"teams"`
		CreatedAt *time.Time `json:"created_at"`
		UpdatedAt *time.Time `json:"updated_at"`
		SSOUserID *string `json:"sso_user_id"`
	}{internalUser: user, Spend: spend, Teams: teams})
}

type userWriteInput struct {
	ownerRateChanges
	ID            string          `json:"user_id"`
	Alias         *string         `json:"user_alias"`
	Email         *string         `json:"user_email"`
	Password      *string         `json:"password"`
	Role          *string         `json:"user_role"`
	Models        *[]string       `json:"models"`
	Metadata      *map[string]any `json:"metadata"`
	AutoCreateKey *bool           `json:"auto_create_key"`
	Organizations []string       `json:"organizations"`
	TeamID        string         `json:"team_id"`
	KeyAlias      string         `json:"key_alias"`
	Duration      string         `json:"duration"`
	MaxBudget     json.RawMessage `json:"max_budget"`
	BudgetDuration json.RawMessage `json:"budget_duration"`
}

func (registry *KeyRegistry) writeUser(w http.ResponseWriter, r *http.Request, update bool) {
	if update && !registry.requireMaster(w, r) {
		return
	}
	var input userWriteInput
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		!input.valid(update) {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported user configuration")
		return
	}
	var passwordHash string
	if input.Password != nil && *input.Password != "" {
		var err error
		passwordHash, err = hashDashboardPassword(*input.Password)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not hash user password")
			return
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	caller, valid := registry.identifyResourceCallerLocked(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin && !registry.canProvisionOrganizationUser(caller.user, input) {
		modelError(w, http.StatusForbidden, "Organization administrator access required")
		return
	}
	if input.ID == "" && update && input.Email != nil && *input.Email != "" {
		for id, user := range registry.state.Users {
			if strings.EqualFold(user.Email, *input.Email) {
				input.ID = id
				break
			}
		}
	}
	current, exists := registry.state.Users[input.ID]
	if update && !exists {
		modelError(w, http.StatusNotFound, "User not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusBadRequest, "User ID already exists")
		return
	}
	var defaultTeams []ssoDefaultTeam
	if defaults := registry.ssoUserDefaultsLocked(); !exists && caller.admin && defaults.enabled() {
		if input.Role == nil {
			input.Role = &defaults.Role
		}
		if input.Models == nil && defaults.Models != nil {
			input.Models = &defaults.Models
		}
		if input.MaxBudget == nil && defaults.MaxBudget != nil {
			input.MaxBudget, _ = json.Marshal(defaults.MaxBudget)
		}
		if input.BudgetDuration == nil && defaults.BudgetDuration != "" {
			input.BudgetDuration, _ = json.Marshal(defaults.BudgetDuration)
		}
		if input.TeamID == "" {
			defaultTeams = defaults.Teams
		}
	}
	if !exists {
		if input.ID == "" {
			input.ID = newEntityID()
		}
		current = internalUser{ID: input.ID, Role: "internal_user", Models: []string{}, Metadata: map[string]any{}}
		if !caller.admin {
			current.DefaultOrgID = input.Organizations[0]
			if input.TeamID != "" {
				current.DefaultOrgID = registry.state.Teams[input.TeamID].OrgID
			}
		}
	}
	if err := registry.applyUserFields(&current, input); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}

	next := registry.state
	next.Users = maps.Clone(next.Users)
	if next.Users == nil {
		next.Users = make(map[string]internalUser)
	}
	next.Users[current.ID] = current
	if len(input.Organizations) > 0 {
		next.Organizations = maps.Clone(next.Organizations)
		seen := make(map[string]bool, len(input.Organizations))
		for _, id := range input.Organizations {
			org, exists := next.Organizations[id]
			if !exists {
				modelError(w, http.StatusBadRequest, "Unknown organization")
				return
			}
			if !seen[id] {
				org.Members = append(slices.Clone(org.Members), organizationMember{
					OrgID: id, UserID: current.ID, Email: current.Email, Role: "internal_user",
				})
				next.Organizations[id] = org
				seen[id] = true
			}
		}
	}
	if input.TeamID != "" {
		defaultTeams = []ssoDefaultTeam{{ID: input.TeamID, Role: "user"}}
	}
	if len(defaultTeams) > 0 {
		next.Teams = maps.Clone(next.Teams)
	}
	for _, assignment := range defaultTeams {
		team, exists := next.Teams[assignment.ID]
		if !exists || team.Blocked {
			modelError(w, http.StatusBadRequest, "Unknown or blocked team")
			return
		}
		role := assignment.Role
		if role == "" {
			role = "user"
		}
		team.Members = append(slices.Clone(team.Members), teamMembership{
			TeamID: team.ID, UserID: current.ID, Email: current.Email, Role: role,
		})
		next.Teams[team.ID] = team
	}
	if input.Password != nil {
		next.setDashboardPassword(current.ID, passwordHash)
	}
	var token string
	if !update && (input.AutoCreateKey == nil || *input.AutoCreateKey) {
		token = newKeyToken()
		now := time.Now().UTC()
		expires, err := keyExpiry(now, input.Duration)
		if err != nil {
			modelError(w, http.StatusBadRequest, "Invalid key duration")
			return
		}
		next.Keys = maps.Clone(next.Keys)
		next.Keys[keyDigest(token)] = virtualKey{UserID: current.ID, TeamID: input.TeamID, Alias: input.KeyAlias,
			OrgID: current.DefaultOrgID, Models: []string{}, CreatedAt: &now, Expires: expires}
	}
	var before any
	action := "created"
	if exists {
		action = "updated"
		before = auditUserSnapshot(registry.state.Users[current.ID])
	}
	if registry.apply(w, next, registry.entityAudit(r, "LiteLLM_UserTable", current.ID, action, before, auditUserSnapshot(current))) {
		current.ImportedSpend = nil
		modelJSON(w, http.StatusOK, struct {
			internalUser
			Key string `json:"key,omitempty"`
		}{internalUser: current, Key: token})
	}
}

func (registry *KeyRegistry) canProvisionOrganizationUser(caller string, input userWriteInput) bool {
	if len(input.Organizations) == 0 || (input.Role != nil && !internalSSORole(*input.Role)) {
		return false
	}
	for _, id := range input.Organizations {
		if organizationRole(registry.state.Organizations[id], caller) != "org_admin" {
			return false
		}
	}
	if input.TeamID != "" {
		team, exists := registry.state.Teams[input.TeamID]
		return exists && slices.Contains(input.Organizations, team.OrgID)
	}
	return true
}

func (input userWriteInput) valid(update bool) bool {
	return (input.Models == nil || validOwnerModels(*input.Models)) &&
		(input.Role == nil || dashboardUserRole(*input.Role)) &&
		(!update || (input.AutoCreateKey == nil && input.Organizations == nil &&
			input.TeamID == "" && input.KeyAlias == "" && input.Duration == ""))
}

func (registry *KeyRegistry) applyUserFields(current *internalUser, input userWriteInput) error {
	if input.Email != nil && *input.Email != "" {
		for _, user := range registry.state.Users {
			if user.ID != current.ID && strings.EqualFold(user.Email, *input.Email) {
				return errors.New("User email already exists")
			}
		}
	}
	if input.Email != nil {
		current.Email = *input.Email
	}
	if input.Alias != nil {
		current.Alias = *input.Alias
	}
	if input.Role != nil {
		current.Role = *input.Role
	}
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}
	if err := registry.updateBudget(input.MaxBudget, input.BudgetDuration, &current.MaxBudget, &current.BudgetDuration, &current.BudgetResetAt); err != nil {
		return err
	}
	if err := input.ownerRateChanges.apply(&current.ownerRates); err != nil {
		return err
	}

	return nil
}

func (registry *KeyRegistry) userInfo(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.identifyReadCaller(r)
	if !ok || (!caller.admin && caller.user == "") {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	id := r.URL.Query().Get("user_id")
	if !caller.admin {
		if id != "" && id != caller.user {
			modelError(w, http.StatusForbidden, "User information is outside the key's scope")
			return
		}
		id = caller.user
	}
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply a user ID")
		return
	}
	registry.mu.RLock()
	user, exists := registry.state.Users[id]
	if caller.admin && id == "default_user_id" && !exists {
		user, exists = internalUser{ID: id, Role: "proxy_admin", Models: []string{}}, true
	}
	keys := make([]keyRecord, 0)
	teams := make([]team, 0)
	for digest, key := range registry.state.Keys {
		if key.UserID == id {
			key.ImportedSpend = nil
			key.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
			keys = append(keys, keyRecord{Token: digest, virtualKey: key})
		}
	}
	for _, owner := range registry.state.Teams {
		if memberIndex(owner, id) >= 0 {
			teams = append(teams, owner.public())
		}
	}
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "User not found")
		return
	}
	user.ImportedSpend = nil
	modelJSON(w, http.StatusOK, map[string]any{"user_id": id, "user_info": user, "keys": keys, "teams": teams})
}

func (registry *KeyRegistry) listUsers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for field := range query {
		switch field {
		case "role", "user_ids", "user_email", "page", "page_size", "sort_by", "sort_order", "team", "organization_ids":
		default:
			modelError(w, http.StatusBadRequest, "Unsupported user list filter")
			return
		}
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 25)
	column, order := query.Get("sort_by"), query.Get("sort_order")
	if pageErr != nil || sizeErr != nil || size > 100 ||
		(column != "" && column != "user_id" && column != "user_email") ||
		(order != "" && order != "asc" && order != "desc") {
		modelError(w, http.StatusBadRequest, "Invalid user list parameters")
		return
	}
	role, email := query.Get("role"), strings.ToLower(query.Get("user_email"))
	rawIDs := query.Get("user_ids")
	ids := strings.Split(rawIDs, ",")
	teamID, orgIDs := query.Get("team"), query.Get("organization_ids")
	registry.mu.RLock()
	caller, valid := registry.identifyReadCallerLocked(r)
	if !valid {
		registry.mu.RUnlock()
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	requestedOrgs := make(map[string]bool)
	for _, id := range strings.Split(orgIDs, ",") {
		requestedOrgs[strings.TrimSpace(id)] = true
	}
	orgUsers := make(map[string]bool)
	scoped := !caller.admin || orgIDs != ""
	if scoped {
		matched := false
		for id, org := range registry.state.Organizations {
			if (orgIDs != "" && !requestedOrgs[id]) ||
				(!caller.admin && organizationRole(org, caller.user) != "org_admin") {
				continue
			}
			matched = true
			for _, member := range org.Members {
				orgUsers[member.UserID] = true
			}
		}
		if !caller.admin && !matched {
			registry.mu.RUnlock()
			modelError(w, http.StatusForbidden, "Organization administrator access required")
			return
		}
	}
	selectedTeam := registry.state.Teams[teamID]
	users := make([]internalUser, 0, len(registry.state.Users))
	for _, user := range registry.state.Users {
		if (role == "" || role == user.Role) && (rawIDs == "" || slices.Contains(ids, user.ID)) &&
			strings.Contains(strings.ToLower(user.Email), email) &&
			(teamID == "" || memberIndex(selectedTeam, user.ID) >= 0) && (!scoped || orgUsers[user.ID]) {
			user.ImportedSpend = nil
			users = append(users, user)
		}
	}
	registry.mu.RUnlock()
	slices.SortFunc(users, func(a, b internalUser) int {
		result := strings.Compare(a.ID, b.ID)
		if column == "user_email" && a.Email != b.Email {
			result = strings.Compare(a.Email, b.Email)
		}
		if order == "desc" {
			return -result
		}
		return result
	})
	total := len(users)
	pages := (total + size - 1) / size
	if page > pages {
		users = users[:0]
	} else {
		start := (page - 1) * size
		users = users[start:min(start+size, total)]
	}
	modelJSON(w, http.StatusOK, map[string]any{"users": users, "total": total, "page": page, "page_size": size, "total_pages": pages})
}

func (registry *KeyRegistry) deleteUsers(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"user_ids"`
		Organization string `json:"organization_id"`
		Organizations []string `json:"organizations"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		len(input.IDs) == 0 || slices.Contains(input.IDs, "") {
		modelError(w, http.StatusBadRequest, "Supply user IDs to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	caller, ok := registry.identifyResourceCallerLocked(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin {
		orgs := append(slices.Clone(input.Organizations), input.Organization)
		for _, id := range orgs {
			if id != "" && organizationRole(registry.state.Organizations[id], caller.user) != "org_admin" {
				modelError(w, http.StatusForbidden, "Organization administrator access required")
				return
			}
		}
		if !registry.canDeleteOrganizationUsers(caller.user, input.IDs) {
			modelError(w, http.StatusForbidden, "User is outside the caller's administered organizations")
			return
		}
	}
	next := registry.state
	next.Users = maps.Clone(next.Users)
	next.Keys = maps.Clone(next.Keys)
	next.Teams = maps.Clone(next.Teams)
	next.Organizations = maps.Clone(next.Organizations)
	count := 0
	var events []*managementAuditEvent
	for _, id := range input.IDs {
		if user, exists := next.Users[id]; exists {
			if registry.storeAuditLogs {
				events = append(events, registry.entityAudit(r, "LiteLLM_UserTable", id, "deleted", auditUserSnapshot(user), nil))
			}
			delete(next.Users, id)
			count++
		}
	}
	next.Passwords = maps.Clone(next.Passwords)
	for _, id := range input.IDs {
		delete(next.Passwords, id)
	}
	next.Invitations = maps.Clone(next.Invitations)
	for id, invitation := range next.Invitations {
		if slices.Contains(input.IDs, invitation.UserID) {
			delete(next.Invitations, id)
		}
	}
	for digest, key := range next.Keys {
		if slices.Contains(input.IDs, key.UserID) {
			delete(next.Keys, digest)
		}
	}
	for id, owner := range next.Teams {
		owner.Members = slices.DeleteFunc(slices.Clone(owner.Members), func(member teamMembership) bool {
			return slices.Contains(input.IDs, member.UserID)
		})
		next.Teams[id] = owner
	}
	for id, org := range next.Organizations {
		org.Members = slices.DeleteFunc(slices.Clone(org.Members), func(member organizationMember) bool {
			return slices.Contains(input.IDs, member.UserID)
		})
		next.Organizations[id] = org
	}
	actor := caller.user
	if caller.admin {
		actor = "default_user_id"
	}
	next.archiveRevokedKeys(registry.state, actor)
	if registry.apply(w, next, events...) {
		modelJSON(w, http.StatusOK, map[string]int{"count": count})
	}
}

func (registry *KeyRegistry) canDeleteOrganizationUsers(caller string, ids []string) bool {
	targets := make(map[string]bool, len(ids))
	for _, id := range ids {
		role := registry.state.Users[id].Role
		if role != "internal_user" && role != "internal_user_viewer" {
			return false
		}
		targets[id] = false
	}
	for _, org := range registry.state.Organizations {
		admin := organizationRole(org, caller) == "org_admin"
		for _, member := range org.Members {
			if _, targeted := targets[member.UserID]; targeted {
				if !admin {
					return false
				}
				targets[member.UserID] = true
			}
		}
	}
	for _, found := range targets {
		if !found {
			return false
		}
	}
	return true
}

func auditUserSnapshot(user internalUser) map[string]any {
	return map[string]any{"user_id": user.ID, "user_role": user.Role, "models": user.Models,
		"max_budget": user.MaxBudget, "budget_duration": user.BudgetDuration}
}
