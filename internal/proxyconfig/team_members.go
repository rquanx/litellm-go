package proxyconfig

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type teamMembership struct {
	TeamID string   `json:"team_id"`
	UserID string   `json:"user_id"`
	Email  string   `json:"user_email,omitempty"`
	Role   string   `json:"role"`
	Models []string `json:"allowed_models"`
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

func (registry *KeyRegistry) writeTeamMembership(w http.ResponseWriter, r *http.Request, operation string) {
	token := runtime.ProxyCredential(r)
	master := subtle.ConstantTimeCompare([]byte(registry.master), []byte(token)) == 1
	var input struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
		Email string `json:"user_email"`
		Role string `json:"role"`
		Member json.RawMessage `json:"member"`
		Models *[]string `json:"allowed_models"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.TeamID == "" ||
		(input.Models != nil && !validOwnerModels(*input.Models)) {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported membership configuration")
		return
	}
	var changes []memberChange
	if operation == "add" {
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
	if !master {
		if _, _, authorized := registry.authorizeLocked(token); !authorized {
			modelError(w, http.StatusUnauthorized, "Proxy token required")
			return
		}
		key := registry.state.Keys[keyDigest(token)]
		if key.RotationTarget != "" {
			key = registry.state.Keys[key.RotationTarget]
		}
		member := memberIndex(current, key.UserID)
		if key.UserID == "" || member < 0 || current.Blocked || current.Members[member].Role != "admin" ||
			(key.TeamID != "" && key.TeamID != current.ID) || key.ProjectID != "" {
			modelError(w, http.StatusForbidden, "Team administrator access required")
			return
		}
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
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
			modelError(w, http.StatusBadRequest, "Invalid membership user or role")
			return
		}
		index := memberIndex(current, user.ID)
		if operation == "add" && (index >= 0 || change.Role == "") {
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
		updatedUsers = append(updatedUsers, user)
		updatedMembers = append(updatedMembers, member)
	}
	next.Teams[current.ID] = current
	if !registry.apply(w, next) {
		return
	}
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
	var spend float64
	if registry.teamMemberSpend != nil {
		var err error
		spend, err = registry.teamMemberSpend(r.Context(), id, caller.user)
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read team member spend")
			return
		}
	}
	var email any
	if user.Email != "" {
		email = user.Email
	}
	modelJSON(w, http.StatusOK, map[string]any{"user_id": caller.user, "team_id": id, "team_alias": owner.Alias,
		"role": member.Role, "user_email": email, "spend": spend, "total_spend": spend, "budget_id": nil, "litellm_budget_table": nil})
}