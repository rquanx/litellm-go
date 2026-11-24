package proxyconfig

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type teamMembership struct {
	TeamID string   `json:"team_id"`
	UserID string   `json:"user_id"`
	Email  string   `json:"user_email,omitempty"`
	Role   string   `json:"role"`
	Models []string `json:"allowed_models"`
	MaxBudget *float64 `json:"max_budget_in_team,omitempty"`
	BudgetDuration string `json:"budget_duration,omitempty"`
	BudgetResetAt *time.Time `json:"budget_reset_at,omitempty"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
}

type memberChange struct {
	UserID string `json:"user_id"`
	Email  string `json:"user_email"`
	Role   string `json:"role"`
}

func readMembershipChanges(raw json.RawMessage) ([]memberChange, error) {
	raw = bytes.TrimSpace(raw)
	if bytes.HasPrefix(raw, []byte("{")) {
		raw = append(append([]byte{'['}, raw...), ']')
	}
	var changes []memberChange
	err := decodeModelJSON(bytes.NewReader(raw), &changes)
	return changes, err
}

func memberIndex(owner team, userID string) int {
	return slices.IndexFunc(owner.Members, func(m teamMembership) bool { return m.UserID == userID })
}

func (owner team) memberBudget(userID string) teamMembership {
	if userID == "" || owner.ID == "" {
		return teamMembership{}
	}
	index := memberIndex(owner, userID)
	var member teamMembership
	if index >= 0 {
		member = owner.Members[index]
		if member.MaxBudget != nil {
			return member
		}
	}
	if owner.MemberBudget != nil && *owner.MemberBudget > 0 {
		member.MaxBudget, member.BudgetDuration, member.BudgetResetAt = owner.MemberBudget, owner.MemberBudgetDuration, owner.MemberBudgetResetAt
	}
	return member
}

func (registry *KeyRegistry) writeTeamMembership(w http.ResponseWriter, r *http.Request, operation string) {
	bulk := r.URL.Path == "/team/bulk_member_add"
	var input struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		Email string `json:"user_email"`
		Role string `json:"role"`
		Member json.RawMessage `json:"member"`
		Members []memberChange `json:"members"`
		AllUsers bool `json:"all_users"`
		Models *[]string `json:"allowed_models"`
		MaxBudget json.RawMessage `json:"max_budget_in_team"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.TeamID == "" ||
		(input.Models != nil && !validOwnerModels(*input.Models)) ||
		(!bulk && (input.AllUsers || input.Members != nil)) ||
		(bulk && input.Member != nil) {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported membership configuration")
		return
	}
	var changes []memberChange
	if bulk {
		changes = input.Members
		if !input.AllUsers && len(changes) == 0 {
			modelError(w, http.StatusBadRequest, "Supply members to add")
			return
		}
	} else if operation == "add" {
		var err error
		changes, err = readMembershipChanges(input.Member)
		if err != nil || len(changes) == 0 {
			modelError(w, http.StatusBadRequest, "Supply members to add")
			return
		}
	} else {
		changes = []memberChange{{UserID: input.UserID, Email: input.Email, Role: input.Role}}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[input.TeamID]
	if !registry.requireTeamAdminLocked(w, r, current) {
		return
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	if bulk && input.AllUsers {
		if subtle.ConstantTimeCompare([]byte(registry.master), []byte(runtime.ProxyCredential(r))) != 1 {
			modelError(w, http.StatusForbidden, "Only proxy admins can add all users to a team")
			return
		}
		changes = make([]memberChange, 0, len(registry.state.Users))
		for _, id := range slices.Sorted(maps.Keys(registry.state.Users)) {
			if memberIndex(current, id) < 0 {
				changes = append(changes, memberChange{UserID: id, Role: "user"})
			}
		}
	}
	if bulk && (len(changes) == 0 || len(changes) > 500) {
		modelError(w, http.StatusBadRequest, "Supply between 1 and 500 members")
		return
	}
	var limit *float64
	if input.MaxBudget != nil && (operation == "delete" || json.Unmarshal(input.MaxBudget, &limit) != nil ||
		(limit != nil && (*limit < 0 || registry.keySpend == nil))) {
		modelError(w, http.StatusBadRequest, "max_budget_in_team must be non-negative and requires PostgreSQL spend tracking")
		return
	}
	current.Members = slices.Clone(current.Members)
	next := registry.state
	next.Teams = maps.Clone(next.Teams)
	next.Users = maps.Clone(next.Users)
	if next.Users == nil {
		next.Users = make(map[string]internalUser)
	}
	if operation == "delete" {
		next.Keys = maps.Clone(next.Keys)
	}
	updatedUsers := make([]internalUser, 0, len(changes))
	updatedMembers := make([]teamMembership, 0, len(changes))
	for _, change := range changes {
		user, ok := membershipUser(next.Users, change.UserID, change.Email, operation == "add")
		if !ok || (change.Role != "" && change.Role != "admin" && change.Role != "user") {
			if bulk {
				writeBulkMemberResponse(w, current, changes, "Invalid membership user or role", nil, nil)
				return
			}
			modelError(w, http.StatusBadRequest, "Invalid membership user or role")
			return
		}
		index := memberIndex(current, user.ID)
		if operation == "add" && (index >= 0 || change.Role == "") {
			if bulk {
				writeBulkMemberResponse(w, current, changes, "Member already exists or has no role", nil, nil)
				return
			}
			modelError(w, http.StatusBadRequest, "Member already exists or has no role")
			return
		}
		if operation != "add" && index < 0 {
			modelError(w, http.StatusNotFound, "Team member not found")
			return
		}
		if operation == "delete" {
			current.Members = slices.Delete(current.Members, index, index+1)
			for digest, key := range next.Keys {
				if key.TeamID == current.ID && key.UserID == user.ID {
					delete(next.Keys, digest)
				}
			}
			continue
		}
		member := teamMembership{TeamID: current.ID, UserID: user.ID, Email: user.Email, Models: []string{}}
		if index >= 0 {
			member = current.Members[index]
		}
		if input.MaxBudget != nil {
			member.MaxBudget = limit
		}
		if change.Role != "" {
			member.Role = change.Role
		}
		if input.Models != nil {
			member.Models = *input.Models
		}
		if operation == "add" {
			current.Members = append(current.Members, member)
			next.Users[user.ID] = user
		} else {
			current.Members[index] = member
		}
		user.ImportedSpend = nil
		updatedUsers = append(updatedUsers, user)
		member.ImportedSpend = nil
		updatedMembers = append(updatedMembers, member)
	}
	next.Teams[current.ID] = current
	if operation == "delete" {
		actor := "default_user_id"
		if caller, ok := registry.identifyResourceCallerLocked(r); ok && !caller.admin {
			actor = caller.user
		}
		next.archiveRevokedKeys(registry.state, actor)
	}
	if bulk {
		if len(updatedMembers) != 0 && !registry.apply(w, next) {
			return
		}
		writeBulkMemberResponse(w, current.public(), changes, "", updatedUsers, updatedMembers)
		return
	}
	if !registry.apply(w, next) {
		return
	}
	current = current.public()
	switch operation {
	case "add":
		modelJSON(w, http.StatusOK, struct {
			team
			Users []internalUser `json:"updated_users"`
			Memberships []teamMembership `json:"updated_team_memberships"`
		}{team: current, Users: updatedUsers, Memberships: updatedMembers})
	case "update":
		modelJSON(w, http.StatusOK, updatedMembers[0])
	default:
		modelJSON(w, http.StatusOK, current)
	}
}

func writeBulkMemberResponse(w http.ResponseWriter, current team, changes []memberChange, failure string,
	users []internalUser, members []teamMembership) {
	results := make([]map[string]any, 0, len(changes))
	success := 0
	for i, change := range changes {
		if failure != "" {
			results = append(results, map[string]any{"user_id": change.UserID, "user_email": change.Email,
				"success": false, "error": failure})
			continue
		}
		user, member := users[i], members[i]
		results = append(results, map[string]any{"user_id": user.ID, "user_email": user.Email,
			"success": true, "updated_user": user, "updated_team_membership": member})
		success++
	}
	var updatedTeam *team
	if success > 0 {
		updatedTeam = &current
	}
	modelJSON(w, http.StatusOK, map[string]any{"team_id": current.ID, "results": results,
		"total_requested": len(changes), "successful_additions": success,
		"failed_additions": len(changes) - success, "updated_team": updatedTeam})
}

func (registry *KeyRegistry) requireTeamAdminLocked(w http.ResponseWriter, r *http.Request, current team) bool {
	token := runtime.ProxyCredential(r)
	if subtle.ConstantTimeCompare([]byte(registry.master), []byte(token)) == 1 {
		return true
	}
	if _, _, authorized := registry.authorizeLocked(token); !authorized {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return false
	}
	key := registry.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key = registry.state.Keys[key.RotationTarget]
	}
	member := memberIndex(current, key.UserID)
	teamAdmin := member >= 0 && current.Members[member].Role == "admin"
	orgAdmin := organizationRole(registry.state.Organizations[current.OrgID], key.UserID) == "org_admin"
	if key.UserID != "" && !current.Blocked && (teamAdmin || orgAdmin) &&
		(key.TeamID == "" || key.TeamID == current.ID) && key.ProjectID == "" {
		return true
	}
	modelError(w, http.StatusForbidden, "Team administrator access required")
	return false
}

func membershipUser(users map[string]internalUser, id, email string, create bool) (internalUser, bool) {
	if id != "" {
		if user, ok := users[id]; ok && (email == "" || strings.EqualFold(email, user.Email)) {
			return user, true
		}
	}
	if email != "" {
		for _, user := range users {
			if strings.EqualFold(email, user.Email) {
				return user, id == "" || user.ID == id
			}
		}
	}
	if !create || (id == "" && email == "") {
		return internalUser{}, false
	}
	if _, exists := users[id]; exists {
		return internalUser{}, false
	}
	if id == "" {
		id = newEntityID()
	}
	return internalUser{ID: id, Email: email, Role: "internal_user", Models: []string{}, Metadata: map[string]any{}}, true
}

func (registry *KeyRegistry) teamMemberMe(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.identifyResourceCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if caller.user == "" {
		modelError(w, http.StatusBadRequest, "API key has no associated user_id")
		return
	}
	id := r.PathValue("team_id")
	registry.mu.RLock()
	owner := registry.state.Teams[id]
	user := registry.state.Users[caller.user]
	index := memberIndex(owner, caller.user)
	var member teamMembership
	if index >= 0 {
		member = owner.Members[index]
	}
	registry.mu.RUnlock()
	if index < 0 {
		modelError(w, http.StatusNotFound, "Team membership not found")
		return
	}
	budget := owner.memberBudget(caller.user)
	start, reset, spend := registry.spendWindow(budget.BudgetDuration, budget.BudgetResetAt, budget.ImportedSpend, time.Now())
	if registry.teamMemberSpend != nil {
		recorded, err := registry.teamMemberSpend(r.Context(), id, caller.user, start)
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read team member spend")
			return
		}
		spend += recorded
	}
	var email any
	if user.Email != "" {
		email = user.Email
	}
	var budgetInfo any
	if budget.MaxBudget != nil {
		budgetInfo = map[string]any{"max_budget": budget.MaxBudget, "budget_duration": budget.BudgetDuration, "budget_reset_at": reset}
	}
	modelJSON(w, http.StatusOK, map[string]any{"user_id": caller.user, "team_id": id, "team_alias": owner.Alias,
		"role": member.Role, "user_email": email, "spend": spend, "total_spend": spend, "budget_id": nil, "litellm_budget_table": budgetInfo})
}
