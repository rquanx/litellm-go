package proxyconfig

import (
	"crypto/subtle"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (registry *KeyRegistry) SetConfiguredModelAccessGroups(config Config) {
	registry.setModelAccessGroups(config.models)
}

func (registry *KeyRegistry) setModelAccessGroups(lists ...[]deployment) {
	if registry == nil {
		return
	}
	groups := make(map[string][]string)
	for _, models := range lists {
		for _, model := range models {
			for _, group := range model.Info.AccessGroups {
				groups[group] = append(groups[group], model.Alias)
			}
		}
	}
	for group, models := range groups {
		slices.Sort(models)
		groups[group] = slices.Compact(models)
	}
	registry.modelAccessGroups.Store(&groups)
}

func (registry *KeyRegistry) modelScope(models []string) []string {
	scope := unrestrictedScope(models)
	groups := registry.modelAccessGroups.Load()
	if groups == nil || len(*groups) == 0 || scope == nil {
		return scope
	}
	var expanded []string
	for _, model := range scope {
		if aliases := (*groups)[model]; len(aliases) > 0 {
			if expanded == nil {
				expanded = slices.Clone(scope)
			}
			for _, alias := range aliases {
				if !slices.Contains(expanded, alias) {
					expanded = append(expanded, alias)
				}
			}
		}
	}
	if expanded != nil {
		return expanded
	}
	return scope
}

type modelAccessGroup struct {
	Name string `json:"access_group"`
	Models []string `json:"model_names"`
	Count int `json:"deployment_count"`
}

func (registry *modelRegistry) registerModelAccessGroups(mux *http.ServeMux) {
	mux.HandleFunc("GET /access_group/list", registry.readModelAccessGroups)
	mux.HandleFunc("GET /access_group/{access_group}/info", registry.readModelAccessGroups)
	mux.HandleFunc("POST /access_group/new", registry.changeModelAccessGroup)
	mux.HandleFunc("PUT /access_group/{access_group}/update", registry.changeModelAccessGroup)
	mux.HandleFunc("DELETE /access_group/{access_group}/delete", registry.changeModelAccessGroup)
}

func (registry *modelRegistry) modelAccessGroupAdmin(w http.ResponseWriter, r *http.Request) bool {
	if registry.token != "" && subtle.ConstantTimeCompare([]byte(registry.token), []byte(runtime.ProxyCredential(r))) == 1 {
		return true
	}
	modelError(w, http.StatusUnauthorized, "Proxy master key required")
	return false
}

func (registry *modelRegistry) readModelAccessGroups(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.modelAccessGroupAdmin(w, r) {
		return
	}
	registry.mu.Lock()
	groups := make(map[string]modelAccessGroup)
	for _, model := range append(slices.Clone(registry.base.models), registry.models...) {
		for _, groupName := range model.Info.AccessGroups {
			group := groups[groupName]
			group.Name = groupName
			group.Count++
			if !slices.Contains(group.Models, model.Alias) {
				group.Models = append(group.Models, model.Alias)
			}
			groups[groupName] = group
		}
	}
	registry.mu.Unlock()
	if name := r.PathValue("access_group"); name != "" {
		group, exists := groups[name]
		if !exists {
			modelError(w, http.StatusNotFound, "Access group not found")
			return
		}
		slices.Sort(group.Models)
		modelJSON(w, http.StatusOK, group)
		return
	}
	result := make([]modelAccessGroup, 0, len(groups))
	for _, name := range slices.Sorted(maps.Keys(groups)) {
		group := groups[name]
		slices.Sort(group.Models)
		result = append(result, group)
	}
	modelJSON(w, http.StatusOK, map[string]any{"access_groups": result})
}

func (registry *modelRegistry) changeModelAccessGroup(w http.ResponseWriter, r *http.Request) {
	if !registry.modelAccessGroupAdmin(w, r) {
		return
	}
	var input struct {
		Name string `json:"access_group"`
		Models []string `json:"model_names"`
		IDs []string `json:"model_ids"`
	}
	deleting := r.Method == http.MethodDelete
	creating := r.Method == http.MethodPost
	if !deleting && decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusBadRequest, "Invalid model access group")
		return
	}
	name := r.PathValue("access_group")
	if creating {
		name = strings.TrimSpace(input.Name)
	}
	if name == "" || (!deleting && len(input.Models)+len(input.IDs) == 0) {
		modelError(w, http.StatusBadRequest, "Supply an access group and model names or IDs")
		return
	}
	if len(input.IDs) > 0 {
		input.Models = nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, model := range registry.base.models {
		if slices.Contains(model.Info.AccessGroups, name) || slices.Contains(input.Models, model.Alias) ||
			slices.Contains(input.IDs, model.Info.ID) {
			modelError(w, http.StatusBadRequest, "Edit static model access groups in the proxy configuration")
			return
		}
	}
	exists := false
	seenNames, seenIDs := make(map[string]bool), make(map[string]bool)
	for _, model := range registry.models {
		exists = exists || slices.Contains(model.Info.AccessGroups, name)
		seenNames[model.Alias], seenIDs[model.Info.ID] = true, true
	}
	if !creating && !exists {
		modelError(w, http.StatusNotFound, "Access group not found")
		return
	}
	if creating && exists {
		modelError(w, http.StatusConflict, "Access group already exists")
		return
	}
	for _, alias := range input.Models {
		if !seenNames[alias] {
			modelError(w, http.StatusBadRequest, "Unknown model name")
			return
		}
	}
	for _, id := range input.IDs {
		if !seenIDs[id] {
			modelError(w, http.StatusBadRequest, "Unknown model ID")
			return
		}
	}
	next := slices.Clone(registry.models)
	updated := 0
	for i, model := range next {
		selected := !deleting && (slices.Contains(input.IDs, model.Info.ID) || slices.Contains(input.Models, model.Alias))
		assigned := slices.Contains(model.Info.AccessGroups, name)
		if selected != assigned {
			next[i].Info.AccessGroups = changeAccessGroupAssignment(model.Info.AccessGroups, name, selected)
		}
		if selected || (deleting && assigned) {
			updated++
		}
	}
	if !registry.apply(w, next) {
		return
	}
	if deleting {
		modelJSON(w, http.StatusOK, map[string]any{"access_group": name, "models_updated": updated,
			"message": "Access group '" + name + "' deleted successfully"})
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"access_group": name, "models_updated": updated,
		"model_names": input.Models, "model_ids": input.IDs})
}
