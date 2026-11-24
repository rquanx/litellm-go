package proxyconfig

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type organization struct {
	ID       string         `json:"organization_id"`
	Alias    string         `json:"organization_alias"`
	Models   []string       `json:"models"`
	Metadata map[string]any `json:"metadata"`
	Members []organizationMember `json:"members"`
	BudgetID string `json:"budget_id,omitempty"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
}

type organizationView struct {
	organization
	Teams   []team `json:"teams"`
}

func (registry *KeyRegistry) registerOrganizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /organization/new", registry.writeOrganization)
	mux.HandleFunc("PATCH /organization/update", registry.writeOrganization)
	mux.HandleFunc("GET /organization/info", registry.organizationInfo)
	mux.HandleFunc("POST /organization/info", registry.organizationInfoBatch)
	mux.HandleFunc("GET /organization/list", registry.listOrganizations)
	mux.HandleFunc("DELETE /organization/delete", registry.deleteOrganizations)
	mux.HandleFunc("POST /organization/member_add", registry.writeOrganizationMember)
	mux.HandleFunc("PATCH /organization/member_update", registry.writeOrganizationMember)
	mux.HandleFunc("DELETE /organization/member_delete", registry.writeOrganizationMember)
}

func (registry *KeyRegistry) writeOrganization(w http.ResponseWriter, r *http.Request) {
	update := r.Method == http.MethodPatch
	if !update && !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		ID       string          `json:"organization_id"`
		Alias    *string         `json:"organization_alias"`
		Models   *[]string       `json:"models"`
		Metadata *map[string]any `json:"metadata"`
		BudgetID json.RawMessage `json:"budget_id"`
	}
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
	if update {
		if _, ok := registry.requireOrganizationAdminLocked(w, r, current); !ok {
			return
		}
	}
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
		metadata := make(map[string]any, len(current.Metadata)+len(*input.Metadata))
		maps.Copy(metadata, current.Metadata)
		maps.Copy(metadata, *input.Metadata)
		current.Metadata = metadata
	}
	if input.BudgetID != nil {
		id, err := registry.validateBudgetID(r.Context(), input.BudgetID)
		if err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
		current.BudgetID = id
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
	org.ImportedSpend = nil
	teams := make([]team, 0)
	for _, entry := range registry.state.Teams {
		if entry.OrgID == org.ID {
			teams = append(teams, entry.public())
		}
	}
	slices.SortFunc(teams, func(a, b team) int { return strings.Compare(a.ID, b.ID) })
	org.Members = append([]organizationMember{}, org.Members...)
	return organizationView{organization: org, Teams: teams}
}

func (registry *KeyRegistry) organizationInfo(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("organization_id")
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply an organization ID")
		return
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	org, exists := registry.state.Organizations[id]
	if !registry.adminViewerLocked(runtime.ProxyCredential(r)) {
		if _, ok := registry.requireOrganizationAdminLocked(w, r, org); !ok {
			return
		}
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Organization not found")
		return
	}
	modelJSON(w, http.StatusOK, registry.organizationView(org))
}

func (registry *KeyRegistry) organizationInfoBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"organizations"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.IDs) == 0 {
		modelError(w, http.StatusBadRequest, "Supply organization IDs to query")
		return
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	results := make([]organization, 0, len(input.IDs))
	seen := make(map[string]bool, len(input.IDs))
	for _, id := range input.IDs {
		org, exists := registry.state.Organizations[id]
		if !registry.adminViewerLocked(runtime.ProxyCredential(r)) {
			if _, ok := registry.requireOrganizationAdminLocked(w, r, org); !ok {
				return
			}
		}
		if exists && !seen[id] {
			org.ImportedSpend = nil
			org.Members = nil
			results = append(results, org)
			seen[id] = true
		}
	}
	modelJSON(w, http.StatusOK, results)
}

func (registry *KeyRegistry) listOrganizations(w http.ResponseWriter, r *http.Request) {
	id, alias := r.URL.Query().Get("org_id"), strings.ToLower(r.URL.Query().Get("org_alias"))
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	caller := memoryCaller{admin: true}
	if !registry.adminViewerLocked(runtime.ProxyCredential(r)) {
		var ok bool
		caller, ok = registry.organizationCallerLocked(w, r)
		if !ok {
			return
		}
	}
	organizations := make([]organizationView, 0, len(registry.state.Organizations))
	for _, currentID := range slices.Sorted(maps.Keys(registry.state.Organizations)) {
		org := registry.state.Organizations[currentID]
		if (caller.admin || organizationRole(org, caller.user) != "") &&
			(id == "" || org.ID == id) && strings.Contains(strings.ToLower(org.Alias), alias) {
			organizations = append(organizations, registry.organizationView(org))
		}
	}
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
	next.pruneAccessGroupTeamOverrides()
	for digest, key := range next.Keys {
		_, teamExists := next.Teams[key.TeamID]
		if slices.Contains(input.IDs, key.OrgID) || (key.TeamID != "" && !teamExists) {
			delete(next.Keys, digest)
		}
	}
	next.archiveRevokedKeys(registry.state, "default_user_id")
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, deleted)
	}
}
