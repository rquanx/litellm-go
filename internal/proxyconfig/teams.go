package proxyconfig

import (
	"crypto/rand"
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
	Models   []string       `json:"models"`
	Blocked  bool           `json:"blocked"`
	Metadata map[string]any `json:"metadata"`
}

type teamChanges struct {
	ID       string          `json:"team_id"`
	Alias    *string         `json:"team_alias"`
	Models   *[]string       `json:"models"`
	Blocked  *bool           `json:"blocked"`
	Metadata *map[string]any `json:"metadata"`
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
	if input.Models != nil && slices.ContainsFunc(*input.Models, func(model string) bool {
		return strings.TrimSpace(model) == "" || model == "all-team-models" || model == "all-proxy-models"
	}) {
		modelError(w, http.StatusBadRequest, "Invalid team model scope")
		return
	}
	if input.ID == "" {
		if update {
			modelError(w, http.StatusBadRequest, "Supply a team ID")
			return
		}
		var id [16]byte
		rand.Read(id[:])
		id[6] = (id[6] & 0x0f) | 0x40
		id[8] = (id[8] & 0x3f) | 0x80
		input.ID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
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
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.Blocked != nil {
		current.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}
	registry.saveTeam(w, current)
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
	if !registry.requireMaster(w, r) {
		return
	}
	id := r.URL.Query().Get("team_id")
	if id == "" {
		modelError(w, http.StatusUnprocessableEntity, "Supply a team ID")
		return
	}
	registry.mu.RLock()
	current, exists := registry.state.Teams[id]
	keys := make([]virtualKey, 0)
	now := time.Now()
	for _, key := range registry.state.Keys {
		if key.TeamID == id && (key.Expires == nil || now.Before(*key.Expires)) {
			keys = append(keys, key)
		}
	}
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"team_id": id, "team_info": current, "keys": keys, "team_memberships": []any{},
	})
}

func (registry *KeyRegistry) listTeams(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	if r.URL.Query().Get("user_id") != "" || r.URL.Query().Get("organization_id") != "" {
		modelError(w, http.StatusBadRequest, "User and organization filters are unsupported")
		return
	}
	registry.mu.RLock()
	teams := make([]team, 0, len(registry.state.Teams))
	for _, id := range slices.Sorted(maps.Keys(registry.state.Teams)) {
		teams = append(teams, registry.state.Teams[id])
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
		if slices.Contains(input.IDs, key.TeamID) {
			delete(next.Keys, digest)
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"count": count})
	}
}
