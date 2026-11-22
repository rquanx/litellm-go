package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

type organization struct {
	ID       string         `json:"organization_id"`
	Alias    string         `json:"organization_alias"`
	Models   []string       `json:"models"`
	Metadata map[string]any `json:"metadata"`
	Members []organizationMember `json:"members"`
}

type organizationView struct {
	organization
	Teams   []team `json:"teams"`
}

func (registry *KeyRegistry) registerOrganizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /organization/new", registry.writeOrganization)
	mux.HandleFunc("PATCH /organization/update", registry.writeOrganization)
	mux.HandleFunc("GET /organization/info", registry.organizationInfo)
	mux.HandleFunc("GET /organization/list", registry.listOrganizations)
	mux.HandleFunc("DELETE /organization/delete", registry.deleteOrganizations)
	mux.HandleFunc("POST /organization/member_add", registry.writeOrganizationMember)
	mux.HandleFunc("PATCH /organization/member_update", registry.writeOrganizationMember)
	mux.HandleFunc("DELETE /organization/member_delete", registry.writeOrganizationMember)
}

func (registry *KeyRegistry) writeOrganization(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		ID       string          `json:"organization_id"`
		Alias    *string         `json:"organization_alias"`
		Models   *[]string       `json:"models"`
		Metadata *map[string]any `json:"metadata"`
	}
	update := r.Method == http.MethodPatch
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		(update && input.ID == "") || (!update && input.Alias == nil) ||
		(input.Alias != nil && strings.TrimSpace(*input.Alias) == "") ||
		(input.Models != nil && !validOwnerModels(*input.Models)) {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported organization configuration")
		return
	}
	if input.ID == "" {
		input.ID = newEntityID()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Organizations[input.ID]
	if update && !exists {
		modelError(w, http.StatusNotFound, "Organization not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusBadRequest, "Organization ID already exists")
		return
	}
	if !exists {
		current = organization{ID: input.ID, Models: []string{}, Metadata: map[string]any{}}
	}
	if input.Alias != nil {
		current.Alias = *input.Alias
	}
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}
	next := registry.state
	next.Organizations = maps.Clone(next.Organizations)
	if next.Organizations == nil {
		next.Organizations = make(map[string]organization)
	}
	next.Organizations[current.ID] = current
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, registry.organizationView(current))
	}
}

func (registry *KeyRegistry) organizationView(org organization) organizationView {
	teams := make([]team, 0)
	for _, entry := range registry.state.Teams {
		if entry.OrgID == org.ID {
			teams = append(teams, entry)
		}
	}
	slices.SortFunc(teams, func(a, b team) int { return strings.Compare(a.ID, b.ID) })
	org.Members = append([]organizationMember{}, org.Members...)
	return organizationView{organization: org, Teams: teams}
}

func (registry *KeyRegistry) organizationInfo(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	id := r.URL.Query().Get("organization_id")
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply an organization ID")
		return
	}
	registry.mu.RLock()
	org, exists := registry.state.Organizations[id]
	if !exists {
		registry.mu.RUnlock()
		modelError(w, http.StatusNotFound, "Organization not found")
		return
	}
	view := registry.organizationView(org)
	registry.mu.RUnlock()
	modelJSON(w, http.StatusOK, view)
}

func (registry *KeyRegistry) listOrganizations(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	id, alias := r.URL.Query().Get("org_id"), strings.ToLower(r.URL.Query().Get("org_alias"))
	registry.mu.RLock()
	organizations := make([]organizationView, 0, len(registry.state.Organizations))
	for _, currentID := range slices.Sorted(maps.Keys(registry.state.Organizations)) {
		org := registry.state.Organizations[currentID]
		if (id == "" || org.ID == id) && strings.Contains(strings.ToLower(org.Alias), alias) {
			organizations = append(organizations, registry.organizationView(org))
		}
	}
	registry.mu.RUnlock()
	modelJSON(w, http.StatusOK, organizations)
}

func (registry *KeyRegistry) deleteOrganizations(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"organization_ids"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.IDs) == 0 {
		modelError(w, http.StatusBadRequest, "Supply organization IDs to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Organizations = maps.Clone(next.Organizations)
	next.Teams = maps.Clone(next.Teams)
	next.Keys = maps.Clone(next.Keys)
	deleted := make([]organizationView, 0, len(input.IDs))
	for _, id := range input.IDs {
		org, exists := next.Organizations[id]
		if !exists {
			modelError(w, http.StatusNotFound, "Organization not found")
			return
		}
		org.Members = []organizationMember{}
		deleted = append(deleted, organizationView{organization: org, Teams: []team{}})
		delete(next.Organizations, id)
	}
	for id, entry := range next.Teams {
		if slices.Contains(input.IDs, entry.OrgID) {
			delete(next.Teams, id)
		}
	}
	for digest, key := range next.Keys {
		_, teamExists := next.Teams[key.TeamID]
		if slices.Contains(input.IDs, key.OrgID) || (key.TeamID != "" && !teamExists) {
			delete(next.Keys, digest)
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, deleted)
	}
}
