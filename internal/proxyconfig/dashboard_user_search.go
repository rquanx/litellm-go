package proxyconfig

import (
	"net/http"
	"slices"
	"strings"
)

func (registry *KeyRegistry) searchDashboardUsers(w http.ResponseWriter, r *http.Request, organizationOnly bool) {
	query := r.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 50)
	if pageErr != nil || sizeErr != nil || size > 100 {
		modelError(w, http.StatusUnprocessableEntity, "Invalid user search pagination")
		return
	}

	type filteredUser struct {
		ID string `json:"user_id"`
		Email *string `json:"user_email"`
	}
	users := []filteredUser{}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	caller, allowed := registry.identifyReadCallerLocked(r)
	if !allowed {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	var allowedUsers map[string]bool
	if organizationOnly && !caller.admin {
		organizations := []organization{}
		if caller.user != "" {
			for _, org := range registry.state.Organizations {
				if organizationMemberIndex(org, caller.user) >= 0 {
					organizations = append(organizations, org)
				}
			}
			if len(organizations) == 0 {
				id := query.Get("team_id")
				if id == "" {
					id = caller.team
				}
				team := registry.state.Teams[id]
				if index := memberIndex(team, caller.user); index >= 0 && team.Members[index].Role == "admin" &&
					(caller.team == "" || caller.team == id) {
					if org, ok := registry.state.Organizations[team.OrgID]; ok {
						organizations = append(organizations, org)
					}
				}
			}
		}
		if len(organizations) == 0 {
			modelError(w, http.StatusForbidden, "User search requires an organization membership or an organization team administrator")
			return
		}
		allowedUsers = make(map[string]bool)
		for _, org := range organizations {
			for _, member := range org.Members {
				allowedUsers[member.UserID] = true
			}
		}
	}

	idFilter, emailFilter := strings.ToLower(query.Get("user_id")), strings.ToLower(query.Get("user_email"))
	for _, user := range registry.state.Users {
		if (allowedUsers != nil && !allowedUsers[user.ID]) || !strings.Contains(strings.ToLower(user.ID), idFilter) ||
			!strings.Contains(strings.ToLower(user.Email), emailFilter) {
			continue
		}
		entry := filteredUser{ID: user.ID}
		if user.Email != "" {
			entry.Email = &user.Email
		}
		users = append(users, entry)
	}
	slices.SortFunc(users, func(a, b filteredUser) int { return strings.Compare(a.ID, b.ID) })
	if page-1 > len(users)/size {
		modelJSON(w, http.StatusOK, users[:0])
		return
	}
	start := (page - 1) * size
	modelJSON(w, http.StatusOK, users[start:min(start+size, len(users))])
}

