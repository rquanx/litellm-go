package proxyconfig

import (
	"maps"
	"net/http"
	"slices"

	"github.com/rquanx/litellm-go/internal/runtime"
)

var teamMemberPermissions = []string{
	"/key/generate", "/key/update", "/key/delete", "/key/regenerate", "/key/service-account/generate",
	"/key/{key_id}/regenerate", "/key/block", "/key/unblock", "/key/bulk_update", "/team/key/bulk_update",
	"/key/{key_id}/reset_spend", "/key/info", "/key/health", "/key/list", "/key/aliases", "/team/daily/activity", "/spend/logs",
}

func validTeamPermissions(permissions []string) bool {
	return !slices.ContainsFunc(permissions, func(route string) bool { return !slices.Contains(teamMemberPermissions, route) })
}

func (entry team) hasMemberPermission(route string) bool {
	return entry.MemberPermissions != nil && slices.Contains(*entry.MemberPermissions, route)
}

func (registry *KeyRegistry) canChangeTeamKeyLocked(caller memoryCaller, target team, route string) bool {
	if caller.admin {
		return true
	}
	if target.ID == "" || target.Blocked || caller.user == "" ||
		(caller.team != "" && caller.team != target.ID) || registry.state.Users[caller.user].Role == "internal_user_viewer" {
		return false
	}
	index := memberIndex(target, caller.user)
	return index >= 0 && (target.Members[index].Role == "admin" || target.hasMemberPermission(route))
}

func (registry *KeyRegistry) teamPermissions(w http.ResponseWriter, r *http.Request) {
	read := r.Method == http.MethodGet
	input := struct {
		ID string `json:"team_id"`
		Permissions []string `json:"team_member_permissions"`
	}{ID: r.URL.Query().Get("team_id")}
	if !read && (decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		input.Permissions == nil || !validTeamPermissions(input.Permissions)) {
		modelError(w, http.StatusBadRequest, "Invalid team member permissions")
		return
	}
	if input.ID == "" {
		modelError(w, http.StatusBadRequest, "Supply a team ID")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, exists := registry.state.Teams[input.ID]
	if !(read && registry.adminViewerLocked(runtime.ProxyCredential(r))) && !registry.requireTeamAdminLocked(w, r, entry) {
		return
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	if read {
		permissions := []string{"/key/info", "/key/health"}
		if entry.MemberPermissions != nil {
			permissions = *entry.MemberPermissions
		}
		modelJSON(w, http.StatusOK, map[string]any{"team_id": entry.ID,
			"team_member_permissions": permissions, "all_available_permissions": teamMemberPermissions})
		return
	}
	entry.MemberPermissions = &input.Permissions
	registry.saveTeam(w, r, entry)
}

func (registry *KeyRegistry) bulkTeamPermissions(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"team_ids"`
		Permissions []string `json:"permissions"`
		All bool `json:"apply_to_all_teams"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		!validTeamPermissions(input.Permissions) {
		modelError(w, http.StatusBadRequest, "Invalid team member permissions")
		return
	}
	if len(input.Permissions) == 0 {
		modelJSON(w, http.StatusOK, map[string]any{"message": "No permissions provided", "teams_updated": 0})
		return
	}
	if input.All == (len(input.IDs) > 0) {
		modelError(w, http.StatusBadRequest, "Supply team IDs or select all teams")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Teams = maps.Clone(next.Teams)
	if input.All {
		input.IDs = slices.Collect(maps.Keys(next.Teams))
	}
	count := 0
	for _, id := range input.IDs {
		entry, exists := next.Teams[id]
		if !exists {
			modelError(w, http.StatusNotFound, "Team not found")
			return
		}
		permissions := []string{}
		if entry.MemberPermissions != nil {
			permissions = slices.Clone(*entry.MemberPermissions)
		}
		for _, route := range input.Permissions {
			if !slices.Contains(permissions, route) {
				permissions = append(permissions, route)
			}
		}
		if entry.MemberPermissions == nil || !slices.Equal(*entry.MemberPermissions, permissions) {
			slices.Sort(permissions)
			entry.MemberPermissions = &permissions
			next.Teams[id] = entry
			count++
		}
	}
	if count == 0 || registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{"message": "Team permissions updated successfully",
			"teams_updated": count, "permissions_appended": input.Permissions})
	}
}
