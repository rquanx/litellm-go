package proxyconfig

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
)

type project struct {
	ID string `json:"project_id"`
	Alias string `json:"project_alias"`
	Description string `json:"description"`
	TeamID string `json:"team_id,omitempty"`
	Models []string `json:"models"`
	Blocked bool `json:"blocked"`
	Metadata map[string]any `json:"metadata"`
	ObjectPermission *objectPermission `json:"object_permission,omitempty"`
}

func (registry *KeyRegistry) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /project/new", registry.writeProject)
	mux.HandleFunc("POST /project/update", registry.writeProject)
	mux.HandleFunc("PATCH /project/update", registry.writeProject)
	mux.HandleFunc("GET /project/info", registry.readProjects)
	mux.HandleFunc("GET /project/list", registry.readProjects)
	mux.HandleFunc("POST /project/delete", registry.deleteProjects)
	mux.HandleFunc("DELETE /project/delete", registry.deleteProjects)
}

func (registry *KeyRegistry) writeProject(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		ID string `json:"project_id"`
		Alias *string `json:"project_alias"`
		Description *string `json:"description"`
		TeamID *string `json:"team_id"`
		Models *[]string `json:"models"`
		Blocked *bool `json:"blocked"`
		Metadata *map[string]any `json:"metadata"`
		Permission json.RawMessage `json:"object_permission"`
	}
	update := r.URL.Path == "/project/update"
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		(update && input.ID == "") || (input.Models != nil && !validOwnerModels(*input.Models)) {
		modelError(w, http.StatusBadRequest, "Invalid project configuration")
		return
	}
	if input.ID == "" {
		input.ID = newEntityID()
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, exists := registry.state.Projects[input.ID]
	if update && !exists {
		modelError(w, http.StatusNotFound, "Project not found")
		return
	}
	if !update && exists {
		modelError(w, http.StatusConflict, "Project ID already exists")
		return
	}
	if !exists {
		record = project{ID: input.ID, Models: []string{}, Metadata: map[string]any{}}
	}
	if input.Alias != nil {
		record.Alias = *input.Alias
	}
	if input.Description != nil {
		record.Description = *input.Description
	}
	if input.TeamID != nil && *input.TeamID != record.TeamID {
		if *input.TeamID != "" {
			if _, exists := registry.state.Teams[*input.TeamID]; !exists {
				modelError(w, http.StatusBadRequest, "Project team not found")
				return
			}
		}
		for _, key := range registry.state.Keys {
			if key.ProjectID == record.ID {
				modelError(w, http.StatusConflict, "Project has assigned keys")
				return
			}
		}
		record.TeamID = *input.TeamID
	}
	if input.Models != nil {
		record.Models = *input.Models
	}
	if input.Blocked != nil {
		record.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		record.Metadata = *input.Metadata
	}
	if input.Permission != nil {
		var permission *objectPermission
		if decodeModelJSON(strings.NewReader(string(input.Permission)), &permission) != nil {
			modelError(w, http.StatusBadRequest, "Unsupported project permission")
			return
		}
		record.ObjectPermission = permission
	}
	next := registry.state
	next.Projects = maps.Clone(next.Projects)
	if next.Projects == nil {
		next.Projects = make(map[string]project)
	}
	next.Projects[record.ID] = record
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, record)
	}
}

func (registry *KeyRegistry) readProjects(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if r.URL.Path == "/project/info" {
		record, exists := registry.state.Projects[r.URL.Query().Get("project_id")]
		if !exists {
			modelError(w, http.StatusNotFound, "Project not found")
			return
		}
		modelJSON(w, http.StatusOK, record)
		return
	}
	result := make([]project, 0)
	filter := r.URL.Query().Get("team_id")
	for _, id := range slices.Sorted(maps.Keys(registry.state.Projects)) {
		record := registry.state.Projects[id]
		if filter == "" || filter == record.TeamID {
			result = append(result, record)
		}
	}
	modelJSON(w, http.StatusOK, result)
}

func (registry *KeyRegistry) deleteProjects(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		IDs []string `json:"project_ids"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || len(input.IDs) == 0 || slices.Contains(input.IDs, "") {
		modelError(w, http.StatusBadRequest, "Supply project IDs")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Projects, next.Keys = maps.Clone(next.Projects), maps.Clone(next.Keys)
	for _, id := range input.IDs {
		delete(next.Projects, id)
	}
	for digest, key := range next.Keys {
		if slices.Contains(input.IDs, key.ProjectID) {
			delete(next.Keys, digest)
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"count": len(registry.state.Projects)})
	}
}
