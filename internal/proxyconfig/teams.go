package proxyconfig

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type team struct {
	ID       string         `json:"team_id"`
	Alias    string         `json:"team_alias"`
	OrgID    string         `json:"organization_id,omitempty"`
	Models   []string       `json:"models"`
	Blocked  bool           `json:"blocked"`
	Metadata map[string]any `json:"metadata"`
	ObjectPermission *objectPermission `json:"object_permission,omitempty"`
	Members []teamMembership `json:"members_with_roles,omitempty"`
}

type teamChanges struct {
	ID       string          `json:"team_id"`
	Alias    *string         `json:"team_alias"`
	OrgID    *string         `json:"organization_id"`
	Models   *[]string       `json:"models"`
	Blocked  *bool           `json:"blocked"`
	Metadata *map[string]any `json:"metadata"`
	ObjectPermission json.RawMessage `json:"object_permission"`
}

func (registry *KeyRegistry) registerTeamRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /team/new", func(w http.ResponseWriter, r *http.Request) {
		registry.writeTeam(w, r, false)
	})
	mux.HandleFunc("POST /team/update", func(w http.ResponseWriter, r *http.Request) {
		registry.writeTeam(w, r, true)
	})
	mux.HandleFunc("POST /team/block", func(w http.ResponseWriter, r *http.Request) {
		registry.setTeamBlocked(w, r, true)
	})
	mux.HandleFunc("POST /team/unblock", func(w http.ResponseWriter, r *http.Request) {
		registry.setTeamBlocked(w, r, false)
	})
	mux.HandleFunc("GET /team/info", registry.teamInfo)
	mux.HandleFunc("GET /team/list", registry.listTeams)
	mux.HandleFunc("POST /team/delete", registry.deleteTeams)
	for _, operation := range []string{"add", "update", "delete"} {
		mux.HandleFunc("POST /team/member_"+operation, func(w http.ResponseWriter, r *http.Request) {
			registry.writeTeamMembership(w, r, operation)
		})
	}
}

func (registry *KeyRegistry) writeTeam(w http.ResponseWriter, r *http.Request, update bool) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input teamChanges
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported team configuration")
		return
	}
	if input.Models != nil && !validOwnerModels(*input.Models) {
		modelError(w, http.StatusBadRequest, "Invalid team model scope")
		return
	}
	if input.ID == "" {
		if update {
			modelError(w, http.StatusBadRequest, "Supply a team ID")
			return
		}
		input.ID = newEntityID()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[input.ID]
	if update && !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusBadRequest, "Team ID already exists")
		return
	}
	if !exists {
		current = team{ID: input.ID, Models: []string{}, Metadata: map[string]any{}}
	}
	if input.Alias != nil {
		current.Alias = *input.Alias
	}
	if input.OrgID != nil {
		if *input.OrgID != "" {
			if _, exists := registry.state.Organizations[*input.OrgID]; !exists {
				modelError(w, http.StatusBadRequest, "Organization not found")
				return
			}
		}
		for _, key := range registry.state.Keys {
			if key.TeamID == current.ID && key.OrgID != "" && key.OrgID != *input.OrgID {
				modelError(w, http.StatusConflict, "Team has keys bound to its organization")
				return
			}
		}
		current.OrgID = *input.OrgID
	}
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.ObjectPermission != nil {
		var permission *objectPermission
		if err := decodeModelJSON(strings.NewReader(string(input.ObjectPermission)), &permission); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid or unsupported object permission")
			return
		}
		current.ObjectPermission = permission
	}
	if input.Blocked != nil {
		current.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}
	registry.saveTeam(w, current)
}

func newEntityID() string {
	var id [16]byte
	rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func validOwnerModels(models []string) bool {
	return !slices.ContainsFunc(models, func(model string) bool {
		return strings.TrimSpace(model) == "" || model == "all-team-models" || model == "all-proxy-models"
	})
}

func (registry *KeyRegistry) setTeamBlocked(w http.ResponseWriter, r *http.Request, blocked bool) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		ID string `json:"team_id"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.ID == "" {
		modelError(w, http.StatusBadRequest, "Supply a team ID")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[input.ID]
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	current.Blocked = blocked
	registry.saveTeam(w, current)
}

func (registry *KeyRegistry) saveTeam(w http.ResponseWriter, current team) {
	next := registry.state
	next.Teams = maps.Clone(next.Teams)
	next.Teams[current.ID] = current
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, current)
	}
}

func (registry *KeyRegistry) teamInfo(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.identifyResourceCaller(r)
	if !ok || (!caller.admin && caller.user == "") {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	id := r.URL.Query().Get("team_id")
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply a team ID")
		return
	}
	registry.mu.RLock()
	current, exists := registry.state.Teams[id]
	if !caller.admin && memberIndex(current, caller.user) < 0 {
		registry.mu.RUnlock()
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	keys := make([]virtualKey, 0)
	now := time.Now()
	for _, key := range registry.state.Keys {
		if key.TeamID == id && (caller.admin || key.UserID == caller.user) && (key.Expires == nil || now.Before(*key.Expires)) {
			keys = append(keys, key)
		}
	}
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"team_id": id, "team_info": current, "keys": keys, "team_memberships": append([]teamMembership{}, current.Members...),
	})
}

func (registry *KeyRegistry) listTeams(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.identifyResourceCaller(r)
	if !ok || (!caller.admin && caller.user == "") {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	userID := r.URL.Query().Get("user_id")
	if !caller.admin {
		if userID != "" && userID != caller.user {
			modelError(w, http.StatusForbidden, "Team list is outside the key's user scope")
			return
		}
		userID = caller.user
	}
	orgID := r.URL.Query().Get("organization_id")
	registry.mu.RLock()
	teams := make([]team, 0, len(registry.state.Teams))
	for _, id := range slices.Sorted(maps.Keys(registry.state.Teams)) {
		current := registry.state.Teams[id]
		if (orgID == "" || orgID == current.OrgID) && (userID == "" || memberIndex(current, userID) >= 0) {
			teams = append(teams, current)
		}
	}
	registry.mu.RUnlock()
	modelJSON(w, http.StatusOK, teams)
}

func (registry *KeyRegistry) deleteTeams(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"team_ids"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.IDs) == 0 {
		modelError(w, http.StatusBadRequest, "Supply team IDs to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Teams = maps.Clone(next.Teams)
	next.Keys = maps.Clone(next.Keys)
	next.Projects = maps.Clone(next.Projects)
	count := 0
	for _, id := range input.IDs {
		if id == "" {
			modelError(w, http.StatusBadRequest, "Team IDs must be nonempty")
			return
		}
		if _, exists := next.Teams[id]; exists {
			delete(next.Teams, id)
			count++
		}
	}
	for digest, key := range next.Keys {
		if slices.Contains(input.IDs, key.TeamID) || slices.Contains(input.IDs, next.Projects[key.ProjectID].TeamID) {
			delete(next.Keys, digest)
		}
	}
	for id, project := range next.Projects {
		if slices.Contains(input.IDs, project.TeamID) {
			delete(next.Projects, id)
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"count": count})
	}
}
