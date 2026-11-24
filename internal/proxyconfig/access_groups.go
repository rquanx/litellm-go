package proxyconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type accessGroupGrants struct {
	Name string `json:"access_group_name"`
	Description *string `json:"description"`
	Models []string `json:"access_model_names"`
	MCP []string `json:"access_mcp_server_ids"`
	Agents []string `json:"access_agent_ids"`
}

type accessGroup struct {
	accessGroupGrants
	ID string `json:"access_group_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy *string `json:"created_by"`
	UpdatedBy *string `json:"updated_by"`
}

type accessGroupView struct {
	accessGroup
	Teams []string `json:"assigned_team_ids"`
	Keys []string `json:"assigned_key_ids"`
}

func (registry *KeyRegistry) registerAccessGroupRoutes(mux *http.ServeMux) {
	for _, base := range []string{"/v1/access_group", "/v1/unified_access_group"} {
		mux.HandleFunc("GET "+base, registry.readAccessGroups)
		mux.HandleFunc("POST "+base, registry.writeAccessGroup)
		mux.HandleFunc("GET "+base+"/{access_group_id}", registry.readAccessGroups)
		mux.HandleFunc("PUT "+base+"/{access_group_id}", registry.writeAccessGroup)
		mux.HandleFunc("DELETE "+base+"/{access_group_id}", registry.writeAccessGroup)
	}
}

func (state accessState) validAccessGroupIDs(ids []string) bool {
	for _, id := range ids {
		if _, exists := state.AccessGroups[id]; !exists {
			return false
		}
	}
	return true
}

func (state accessState) accessGroupView(group accessGroup) accessGroupView {
	view := accessGroupView{accessGroup: group, Teams: []string{}, Keys: []string{}}
	for id, entry := range state.Teams {
		if slices.Contains(entry.AccessGroupIDs, group.ID) {
			view.Teams = append(view.Teams, id)
		}
	}
	for id, key := range state.Keys {
		if key.RotationTarget == "" && slices.Contains(key.AccessGroupIDs, group.ID) &&
			!slices.Contains(key.RestrictedGroupIDs, group.ID) {
			view.Keys = append(view.Keys, id)
		}
	}
	slices.Sort(view.Teams)
	slices.Sort(view.Keys)
	return view
}

func (registry *KeyRegistry) readAccessGroups(w http.ResponseWriter, r *http.Request) {
	if !registry.requireAdminRead(w, r) {
		return
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if id := r.PathValue("access_group_id"); id != "" {
		group, exists := registry.state.AccessGroups[id]
		if !exists {
			modelError(w, http.StatusNotFound, "Access group not found")
			return
		}
		modelJSON(w, http.StatusOK, registry.state.accessGroupView(group))
		return
	}
	views := make([]accessGroupView, 0, len(registry.state.AccessGroups))
	indices := make(map[string]int, len(registry.state.AccessGroups))
	for _, group := range registry.state.AccessGroups {
		indices[group.ID] = len(views)
		views = append(views, accessGroupView{accessGroup: group, Teams: []string{}, Keys: []string{}})
	}
	for _, teamID := range slices.Sorted(maps.Keys(registry.state.Teams)) {
		for _, id := range registry.state.Teams[teamID].AccessGroupIDs {
			if index, ok := indices[id]; ok {
				views[index].Teams = append(views[index].Teams, teamID)
			}
		}
	}
	for _, digest := range slices.Sorted(maps.Keys(registry.state.Keys)) {
		key := registry.state.Keys[digest]
		if key.RotationTarget != "" {
			continue
		}
		for _, id := range key.AccessGroupIDs {
			if index, ok := indices[id]; ok && !slices.Contains(key.RestrictedGroupIDs, id) {
				views[index].Keys = append(views[index].Keys, digest)
			}
		}
	}
	for i := range views {
		views[i].Teams = slices.Compact(views[i].Teams)
		views[i].Keys = slices.Compact(views[i].Keys)
	}
	slices.SortFunc(views, func(a, b accessGroupView) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	modelJSON(w, http.StatusOK, views)
}

func (registry *KeyRegistry) writeAccessGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var body []byte
	if r.Method != http.MethodDelete {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			modelError(w, http.StatusBadRequest, "Invalid access group request")
			return
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	state := registry.state
	id := r.PathValue("access_group_id")
	group, exists := state.AccessGroups[id]
	creating := r.Method == http.MethodPost
	if !creating && !exists {
		modelError(w, http.StatusNotFound, "Access group not found")
		return
	}
	state.AccessGroups = maps.Clone(state.AccessGroups)
	if state.AccessGroups == nil {
		state.AccessGroups = make(map[string]accessGroup)
	}
	if r.Method == http.MethodDelete {
		delete(state.AccessGroups, id)
		state.AccessGroupTeamOverrides = maps.Clone(state.AccessGroupTeamOverrides)
		delete(state.AccessGroupTeamOverrides, id)
		state.assignAccessGroup(id, nil, nil)
		if registry.apply(w, state) {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	current := state.accessGroupView(group)
	input := struct {
		accessGroupGrants
		Teams json.RawMessage `json:"assigned_team_ids"`
		Keys json.RawMessage `json:"assigned_key_ids"`
	}{
		accessGroupGrants: group.accessGroupGrants,
	}
	input.Models = slices.Clone(input.Models)
	input.MCP = slices.Clone(input.MCP)
	input.Agents = slices.Clone(input.Agents)
	if err := decodeModelJSON(bytes.NewReader(body), &input); err != nil ||
		strings.TrimSpace(input.Name) == "" || !validOwnerModels(input.Models) {
		modelError(w, http.StatusBadRequest, "Invalid access group")
		return
	}
	for otherID, other := range state.AccessGroups {
		if otherID != id && other.Name == input.Name {
			modelError(w, http.StatusConflict, "Access group name already exists")
			return
		}
	}
	var keyReferences []string
	if input.Keys != nil && json.Unmarshal(input.Keys, &keyReferences) != nil {
		modelError(w, http.StatusBadRequest, "Invalid assigned keys")
		return
	}
	memberKeys := make(map[string]bool, len(keyReferences))
	for _, reference := range keyReferences {
		digest := registry.resolveKey(reference)
		key, found := state.Keys[digest]
		if !found || key.RotationTarget != "" {
			modelError(w, http.StatusBadRequest, "Unknown virtual key")
			return
		}
		memberKeys[digest] = true
	}
	teamReferences := current.Teams
	if input.Teams != nil && json.Unmarshal(input.Teams, &teamReferences) != nil {
		modelError(w, http.StatusBadRequest, "Invalid assigned teams")
		return
	}
	memberTeams := make(map[string]bool, len(teamReferences))
	for _, team := range teamReferences {
		if _, found := state.Teams[team]; !found {
			modelError(w, http.StatusBadRequest, "Unknown team")
			return
		}
		memberTeams[team] = true
	}
	now := time.Now().UTC()
	actor := "default_user_id"
	status := http.StatusOK
	if creating {
		id = newEntityID()
		group.ID, group.CreatedAt, group.CreatedBy = id, now, &actor
		status = http.StatusCreated
	}
	group.accessGroupGrants = input.accessGroupGrants
	group.Models = append([]string{}, group.Models...)
	group.MCP = append([]string{}, group.MCP...)
	group.Agents = append([]string{}, group.Agents...)
	group.UpdatedAt, group.UpdatedBy = now, &actor
	state.AccessGroups[id] = group
	if input.Keys == nil {
		memberKeys = nil
	}
	if input.Teams != nil {
		state.AccessGroupTeamOverrides = maps.Clone(state.AccessGroupTeamOverrides)
		delete(state.AccessGroupTeamOverrides, id)
	}
	state.assignAccessGroup(id, memberTeams, memberKeys)
	if registry.apply(w, state) {
		modelJSON(w, status, state.accessGroupView(group))
	}
}

func (state accessState) keyGroupTeamModels(key virtualKey) []string {
	var models []string
	for _, id := range key.AccessGroupIDs {
		group := state.AccessGroups[id]
		if !slices.Contains(key.RestrictedGroupIDs, id) ||
			(key.TeamID != "" && slices.Contains(state.AccessGroupTeamOverrides[id], key.TeamID)) {
			models = append(models, group.Models...)
		}
	}
	return models
}

func (state *accessState) pruneAccessGroupTeamOverrides() {
	if len(state.AccessGroupTeamOverrides) == 0 {
		return
	}
	state.AccessGroupTeamOverrides = maps.Clone(state.AccessGroupTeamOverrides)
	for group, teams := range state.AccessGroupTeamOverrides {
		remaining := slices.DeleteFunc(slices.Clone(teams), func(id string) bool {
			_, exists := state.Teams[id]
			return !exists
		})
		if len(remaining) == 0 {
			delete(state.AccessGroupTeamOverrides, group)
		} else {
			state.AccessGroupTeamOverrides[group] = remaining
		}
	}
}

func (state *accessState) assignAccessGroup(id string, teams, keys map[string]bool) {
	state.Teams = maps.Clone(state.Teams)
	for teamID, entry := range state.Teams {
		entry.AccessGroupIDs = changeAccessGroupAssignment(entry.AccessGroupIDs, id, teams[teamID])
		state.Teams[teamID] = entry
	}
	if _, exists := state.AccessGroups[id]; exists && keys == nil {
		return
	}
	state.Keys = maps.Clone(state.Keys)
	for digest, key := range state.Keys {
		key.AccessGroupIDs = changeAccessGroupAssignment(key.AccessGroupIDs, id, keys[digest])
		key.RestrictedGroupIDs = changeAccessGroupAssignment(key.RestrictedGroupIDs, id, false)
		state.Keys[digest] = key
	}
}

func decodeImportedAccessGroup(payload []byte) (accessGroupView, error) {
	var record struct {
		accessGroupView
		Created json.RawMessage `json:"created_at"`
		Updated json.RawMessage `json:"updated_at"`
	}
	err := json.Unmarshal(payload, &record)
	group := record.accessGroupView
	created, createdErr := liteLLMTimestamp(record.Created)
	updated, updatedErr := liteLLMTimestamp(record.Updated)
	if err != nil || createdErr != nil || updatedErr != nil || created == nil || updated == nil ||
		group.ID == "" || strings.TrimSpace(group.Name) == "" || !validOwnerModels(group.Models) {
		return group, errors.New("invalid access group")
	}
	group.CreatedAt, group.UpdatedAt = *created, *updated
	group.Models = append([]string{}, group.Models...)
	group.MCP = append([]string{}, group.MCP...)
	group.Agents = append([]string{}, group.Agents...)
	return group, nil
}

func changeAccessGroupAssignment(ids []string, id string, assign bool) []string {
	contains := slices.Contains(ids, id)
	if assign == contains {
		return ids
	}
	if assign {
		return append(slices.Clone(ids), id)
	}
	return slices.DeleteFunc(slices.Clone(ids), func(existing string) bool { return existing == id })
}

func (state accessState) groupGrants(ids []string) accessGroupGrants {
	var grants accessGroupGrants
	for _, id := range ids {
		group := state.AccessGroups[id]
		grants.Models = append(grants.Models, group.Models...)
		grants.MCP = append(grants.MCP, group.MCP...)
		grants.Agents = append(grants.Agents, group.Agents...)
	}
	return grants
}

func addGroupModels(scope, grants []string) []string {
	if scope == nil || len(grants) == 0 {
		return scope
	}
	return unrestrictedScope(append(slices.Clone(scope), grants...))
}

func addGroupObjectPermissions(permission *objectPermission, grants accessGroupGrants) *objectPermission {
	if len(grants.MCP)+len(grants.Agents) == 0 {
		return permission
	}
	var combined objectPermission
	if permission != nil {
		combined = *permission
	}
	if len(grants.MCP) > 0 {
		combined.MCPServers = append(slices.Clone(combined.MCPServers), grants.MCP...)
	}
	combined.Agents = append(slices.Clone(combined.Agents), grants.Agents...)
	return &combined
}
