package proxyconfig

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

type virtualKey struct {
	Alias     string         `json:"key_alias,omitempty"`
	Models    []string       `json:"models"`
	Expires   *time.Time     `json:"expires,omitempty"`
	TeamID    string         `json:"team_id,omitempty"`
	Blocked   bool           `json:"blocked"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
}

type accessState struct {
	Keys  map[string]virtualKey `json:"keys"`
	Teams map[string]team       `json:"teams"`
}

func (state *accessState) UnmarshalJSON(payload []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return errors.New("invalid access state")
	}
	if _, exists := fields["keys"]; !exists {
		return json.Unmarshal(payload, &state.Keys)
	}
	type stateJSON accessState
	return json.Unmarshal(payload, (*stateJSON)(state))
}

type KeyRegistry struct {
	master string
	store  stateStore
	mu     sync.RWMutex
	state  accessState
}

func LoadKeys(master, path string) (*KeyRegistry, error) {
	return loadKeys(master, chooseStateStore(path, nil, "keys"))
}

func LoadPostgresKeys(master string, db *sql.DB) (*KeyRegistry, error) {
	return loadKeys(master, chooseStateStore("", db, "keys"))
}

func loadKeys(master string, store stateStore) (*KeyRegistry, error) {
	if master == "" {
		return nil, errors.New("virtual keys require a proxy master key")
	}
	registry := &KeyRegistry{master: master, store: store, state: accessState{
		Keys: make(map[string]virtualKey), Teams: make(map[string]team),
	}}
	if store == nil {
		return registry, nil
	}
	if err := store.load(&registry.state); err != nil || registry.state.Keys == nil || registry.state.Teams == nil {
		return nil, errors.New("invalid virtual key state")
	}
	return registry, nil
}

func (registry *KeyRegistry) Authorize(token string) ([]string, bool) {
	if token == "" {
		return nil, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	key, exists := registry.state.Keys[keyDigest(token)]
	if !exists || key.Blocked || (key.Expires != nil && !time.Now().Before(*key.Expires)) {
		return nil, false
	}
	if key.TeamID == "" {
		return unrestrictedScope(key.Models), true
	}
	owner, exists := registry.state.Teams[key.TeamID]
	if !exists || owner.Blocked {
		return nil, false
	}
	return intersectScopes(key.Models, owner.Models), true
}

func intersectScopes(key, team []string) []string {
	key = unrestrictedScope(key)
	team = unrestrictedScope(team)
	switch {
	case key == nil || slices.Contains(key, "all-team-models"):
		return team
	case team == nil:
		return key
	}
	scope := make([]string, 0, len(key))
	for _, model := range key {
		if slices.Contains(team, model) {
			scope = append(scope, model)
		}
	}
	return scope
}

func unrestrictedScope(models []string) []string {
	if len(models) == 0 || slices.Contains(models, "*") {
		return nil
	}
	return models
}

func (registry *KeyRegistry) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /key/generate", registry.generate)
	mux.HandleFunc("GET /key/info", registry.info)
	mux.HandleFunc("POST /key/delete", registry.remove)
	mux.HandleFunc("POST /key/update", registry.updateKey)
	mux.HandleFunc("POST /key/block", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, true)
	})
	mux.HandleFunc("POST /key/unblock", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, false)
	})
	mux.HandleFunc("GET /key/list", registry.listKeys)
	registry.registerTeamRoutes(mux)
	mux.Handle("/", next)
	return mux
}

func (registry *KeyRegistry) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte("Bearer "+registry.master), []byte(r.Header.Get("Authorization"))) == 1 {
		return true
	}
	modelError(w, http.StatusUnauthorized, "Proxy master key required")
	return false
}

func (registry *KeyRegistry) generate(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Alias    string         `json:"key_alias"`
		Duration string         `json:"duration"`
		Models   []string       `json:"models"`
		TeamID   string         `json:"team_id"`
		Blocked  bool           `json:"blocked"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key configuration")
		return
	}
	now := time.Now().UTC()
	expiry, err := keyExpiry(now, input.Duration)
	if err != nil {
		modelError(w, http.StatusBadRequest, "Invalid key duration")
		return
	}
	key := virtualKey{Alias: input.Alias, Models: append([]string{}, input.Models...), TeamID: input.TeamID,
		Blocked: input.Blocked, Metadata: input.Metadata, Expires: expiry, CreatedAt: &now}
	var random [32]byte
	rand.Read(random[:])
	token := "sk-" + hex.EncodeToString(random[:])
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if !registry.validKey(w, key) {
		return
	}
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	digest := keyDigest(token)
	next.Keys[digest] = key
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{"key": token, "token_id": digest, "expires": key.Expires, "key_alias": key.Alias})
	}
}

func (registry *KeyRegistry) info(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	token := r.URL.Query().Get("key")
	registry.mu.RLock()
	digest := registry.resolveKey(token)
	key, exists := registry.state.Keys[digest]
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"key": digest, "info": key})
}

func (registry *KeyRegistry) remove(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Keys []string `json:"keys"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || len(input.Keys) == 0 {
		modelError(w, http.StatusBadRequest, "Supply virtual keys to delete")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	removed := 0
	for _, token := range input.Keys {
		digest := registry.resolveKey(token)
		if _, exists := next.Keys[digest]; exists {
			delete(next.Keys, digest)
			removed++
		}
	}
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]int{"deleted_keys": removed})
	}
}

func (registry *KeyRegistry) apply(w http.ResponseWriter, next accessState) bool {
	if registry.store != nil {
		if err := registry.store.save(next); err != nil {
			stateWriteError(w, err)
			return false
		}
	}
	registry.state = next
	return true
}

func keyDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
