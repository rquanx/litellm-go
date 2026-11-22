package proxyconfig

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
)

type organizationMember struct {
	OrgID  string `json:"organization_id"`
	UserID string `json:"user_id"`
	Email  string `json:"user_email,omitempty"`
	Role   string `json:"user_role"`
}

func organizationMemberIndex(org organization, userID string) int {
	return slices.IndexFunc(org.Members, func(member organizationMember) bool { return member.UserID == userID })
}

func (registry *KeyRegistry) writeOrganizationMember(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		OrgID  string          `json:"organization_id"`
		UserID string          `json:"user_id"`
		Email  string          `json:"user_email"`
		Role   string          `json:"role"`
		Member json.RawMessage `json:"member"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.OrgID == "" {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported organization membership")
		return
	}
	add := r.Method == http.MethodPost
	remove := r.Method == http.MethodDelete
	changes := []memberChange{{UserID: input.UserID, Email: input.Email, Role: input.Role}}
	if add {
		var err error
		changes, err = readMembershipChanges(input.Member)
		if err != nil || len(changes) == 0 {
			modelError(w, http.StatusBadRequest, "Supply organization members to add")
			return
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	org, exists := registry.state.Organizations[input.OrgID]
	if !exists {
		modelError(w, http.StatusNotFound, "Organization not found")
		return
	}
	org.Members = slices.Clone(org.Members)
	next := registry.state
	next.Organizations = maps.Clone(next.Organizations)
	if add {
		next.Users = maps.Clone(next.Users)
		if next.Users == nil {
			next.Users = make(map[string]internalUser)
		}
	}
	if remove {
		next.Keys = maps.Clone(next.Keys)
	}
	users := make([]internalUser, 0, len(changes))
	members := make([]organizationMember, 0, len(changes))
	for _, change := range changes {
		user, ok := membershipUser(next.Users, change.UserID, change.Email, add)
		if !ok || (change.Role != "" && !slices.Contains([]string{"org_admin", "internal_user", "internal_user_viewer"}, change.Role)) {
			modelError(w, http.StatusBadRequest, "Invalid organization member or role")
			return
		}
		index := organizationMemberIndex(org, user.ID)
		if add && (index >= 0 || change.Role == "") {
			modelError(w, http.StatusBadRequest, "Organization member already exists or has no role")
			return
		}
		if !add && index < 0 {
			modelError(w, http.StatusNotFound, "Organization member not found")
			return
		}
		member := organizationMember{OrgID: org.ID, UserID: user.ID, Email: user.Email, Role: change.Role}
		switch {
		case add:
			next.Users[user.ID] = user
			org.Members = append(org.Members, member)
		case remove:
			member = org.Members[index]
			org.Members = slices.Delete(org.Members, index, index+1)
			for digest, key := range next.Keys {
				if key.UserID == user.ID && (key.OrgID == org.ID || next.Teams[key.TeamID].OrgID == org.ID) {
					delete(next.Keys, digest)
				}
			}
		default:
			if change.Role == "" {
				member.Role = org.Members[index].Role
			}
			org.Members[index] = member
		}
		users = append(users, user)
		members = append(members, member)
	}
	next.Organizations[org.ID] = org
	if !registry.apply(w, next) {
		return
	}
	if add {
		modelJSON(w, http.StatusOK, map[string]any{"organization_id": org.ID, "updated_users": users,
			"updated_organization_memberships": members})
		return
	}
	modelJSON(w, http.StatusOK, members[0])
}
