package proxyconfig

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (registry *KeyRegistry) keyInfoBatch(w http.ResponseWriter, r *http.Request) {
	caller, valid := registry.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	callerDigest := keyDigest(runtime.ProxyCredential(r))
	var input struct {
		Keys    []string `json:"keys"`
		Aliases []string `json:"key_aliases"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid key information request")
		return
	}
	registry.mu.RLock()
	selected := make(map[string]bool, len(input.Keys))
	for _, key := range input.Keys {
		selected[registry.resolveKey(key)] = true
	}
	aliases := make(map[string]bool, len(input.Aliases))
	for _, alias := range input.Aliases {
		if alias != "" {
			aliases[alias] = true
		}
	}
	if len(aliases) != 0 {
		for digest, key := range registry.state.Keys {
			if aliases[key.Alias] {
				selected[digest] = true
			}
		}
	}
	keys := make([]virtualKey, 0, len(selected))
	digests := make([]string, 0, len(selected))
	for digest := range selected {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	for _, digest := range digests {
		if key, exists := registry.state.Keys[digest]; exists && registry.canReadKeyLocked(caller, callerDigest, digest, key) {
			key.ImportedSpend = nil
			keys = append(keys, key)
		}
	}
	registry.mu.RUnlock()
	modelJSON(w, http.StatusOK, map[string]any{"key": input.Keys, "info": keys})
}

func (registry *KeyRegistry) canReadKeyLocked(caller memoryCaller, callerDigest, digest string, key virtualKey) bool {
	if caller.admin || callerDigest == digest {
		return true
	}
	if caller.user == "" {
		return false
	}
	if key.UserID == caller.user {
		return true
	}
	teamID := key.TeamID
	if teamID == "" && key.ProjectID != "" {
		teamID = registry.state.Projects[key.ProjectID].TeamID
	}
	entry, exists := registry.state.Teams[teamID]
	return exists && !entry.Blocked && memberIndex(entry, caller.user) >= 0
}

func (registry *KeyRegistry) keyAliases(w http.ResponseWriter, r *http.Request) {
	caller, valid := registry.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	query := r.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "size", 50)
	if pageErr != nil || sizeErr != nil || size > 100 {
		modelError(w, http.StatusUnprocessableEntity, "Invalid page or size")
		return
	}
	search, filterTeam := strings.ToLower(query.Get("search")), query.Get("team_id")
	registry.mu.RLock()
	teams := make(map[string]bool)
	if !caller.admin && caller.user != "" {
		for id, entry := range registry.state.Teams {
			if !entry.Blocked && memberIndex(entry, caller.user) >= 0 {
				teams[id] = true
			}
		}
	}
	aliases := []string{}
	for _, key := range registry.state.Keys {
		teamID := key.TeamID
		if teamID == "" && key.ProjectID != "" {
			teamID = registry.state.Projects[key.ProjectID].TeamID
		}
		if key.Alias == "" || teamID == "litellm-dashboard" || strings.HasPrefix(key.Alias, "ui-sso:") ||
			(filterTeam != "" && filterTeam != teamID) || !strings.Contains(strings.ToLower(key.Alias), search) {
			continue
		}
		if caller.admin || (caller.user != "" && (caller.user == key.UserID || teams[teamID])) {
			aliases = append(aliases, key.Alias)
		}
	}
	registry.mu.RUnlock()
	slices.Sort(aliases)
	count := len(aliases)
	pages := (count + size - 1) / size
	if page > pages {
		aliases = aliases[:0]
	} else {
		start := (page - 1) * size
		aliases = aliases[start:min(count, start+size)]
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"aliases": aliases, "total_count": count, "current_page": page, "total_pages": pages, "size": size,
	})
}
