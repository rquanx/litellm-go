package proxyconfig

import (
	"context"
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

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type virtualKey struct {
	Alias     string         `json:"key_alias,omitempty"`
	Models    []string       `json:"models"`
	Expires   *time.Time     `json:"expires,omitempty"`
	TeamID    string         `json:"team_id,omitempty"`
	ProjectID string         `json:"project_id,omitempty"`
	OrgID     string         `json:"organization_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	Blocked   bool           `json:"blocked"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	ObjectPermission *objectPermission `json:"object_permission,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	RotationTarget string     `json:"rotation_target,omitempty"`
}

type accessState struct {
	Keys  map[string]virtualKey `json:"keys"`
	Teams map[string]team       `json:"teams"`
	Organizations map[string]organization `json:"organizations,omitempty"`
	Users map[string]internalUser `json:"users,omitempty"`
	Projects map[string]project `json:"projects,omitempty"`
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
	ssoDefaults *SSOUserDefaults
	teamMemberSpend func(context.Context, string, string) (float64, error)
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
		Keys: make(map[string]virtualKey), Teams: make(map[string]team), Organizations: make(map[string]organization),
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
	models, _, valid := registry.authorize(token)
	return models, valid
}

func (registry *KeyRegistry) AuthorizeSearch(token string) ([]string, bool) {
	_, permissions, valid := registry.authorize(token)
	return intersectScopes(intersectScopes(permissions.key.scope(), permissions.team.scope()), permissions.project.scope()), valid
}

func (registry *KeyRegistry) AuthorizeMCP(token string) (runtime.MCPAccess, bool) {
	_, permissions, valid := registry.authorize(token)
	if !valid {
		return nil, false
	}
	return func(server runtime.MCPServer, tool string) bool {
		key, team := permissions.key, permissions.team
		serverAllowed := team.grantsMCPServer(server)
		if key.hasMCPGrants() {
			serverAllowed = key.grantsMCPServer(server) && (!team.hasMCPGrants() || serverAllowed)
		}
		serverAllowed = serverAllowed || server.AllowAllKeys
		project := permissions.project
		if project.hasMCPGrants() {
			serverAllowed = serverAllowed && project.grantsMCPServer(server)
		}
		return serverAllowed && key.allowsMCPTool(server, tool) && team.allowsMCPTool(server, tool) && project.allowsMCPTool(server, tool)
	}, true
}

type objectPermission struct {
	Agents []string `json:"agents,omitempty"`
	AgentGroups []string `json:"agent_access_groups,omitempty"`
	SearchTools []string `json:"search_tools,omitempty"`
	MCPServers []string `json:"mcp_servers"`
	MCPTools map[string][]string `json:"mcp_tool_permissions,omitempty"`
	MCPAccessGroups []string `json:"mcp_access_groups,omitempty"`
	MCPToolsets []string `json:"mcp_toolsets,omitempty"`
}

func (registry *KeyRegistry) AuthorizeMCPToolset(token string) ([]string, runtime.MCPAccess, bool) {
	_, permissions, valid := registry.authorize(token)
	if !valid {
		return nil, nil, false
	}
	var key, team []string
	if permissions.key != nil && len(permissions.key.MCPToolsets) != 0 {
		key = permissions.key.MCPToolsets
	}
	if permissions.team != nil && len(permissions.team.MCPToolsets) != 0 {
		team = permissions.team.MCPToolsets
	}
	grants := intersectScopes(key, team)
	if permissions.project != nil && len(permissions.project.MCPToolsets) != 0 {
		grants = intersectScopes(grants, permissions.project.MCPToolsets)
	}
	return grants, func(server runtime.MCPServer, tool string) bool {
		project := permissions.project
		return (!project.hasMCPGrants() || project.grantsMCPServer(server)) && project.allowsMCPTool(server, tool) &&
			permissions.key.allowsMCPTool(server, tool) && permissions.team.allowsMCPTool(server, tool)
	}, true
}

func (permission *objectPermission) hasMCPGrants() bool {
	return permission != nil && (permission.MCPServers != nil || len(permission.MCPAccessGroups) > 0)
}

func (permission *objectPermission) grantsMCPServer(server runtime.MCPServer) bool {
	if permission == nil {
		return false
	}
	for _, id := range permission.MCPServers {
		if id == server.ID || id == server.Name || (server.Alias != "" && id == server.Alias) {
			return true
		}
	}
	for _, group := range server.AccessGroups {
		if slices.Contains(permission.MCPAccessGroups, group) {
			return true
		}
	}
	return false
}

func (permission *objectPermission) allowsMCPTool(server runtime.MCPServer, tool string) bool {
	if permission == nil || tool == "" {
		return true
	}
	for _, id := range []string{server.ID, server.Name, server.Alias} {
		if id != "" && len(permission.MCPTools[id]) != 0 && !runtime.MCPToolMatches(permission.MCPTools[id], tool) {
			return false
		}
	}
	return true
}

type keyObjectPermissions struct {
	key, team, project *objectPermission
}

func (permission *objectPermission) scope() []string {
	if permission == nil || len(permission.SearchTools) == 0 {
		return nil
	}
	return permission.SearchTools
}

func (registry *KeyRegistry) authorize(token string) ([]string, keyObjectPermissions, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.authorizeLocked(token)
}

func (registry *KeyRegistry) authorizeLocked(token string) ([]string, keyObjectPermissions, bool) {
	var permissions keyObjectPermissions
	if token == "" {
		return nil, permissions, false
	}
	key, exists := registry.state.Keys[keyDigest(token)]
	if !exists || key.Blocked || (key.Expires != nil && !time.Now().Before(*key.Expires)) {
		return nil, permissions, false
	}
	if key.RotationTarget != "" {
		key, exists = registry.state.Keys[key.RotationTarget]
		if !exists || key.Blocked || key.RotationTarget != "" || (key.Expires != nil && !time.Now().Before(*key.Expires)) {
			return nil, permissions, false
		}
	}
	permissions.key = key.ObjectPermission
	var projectModels []string
	if key.ProjectID != "" {
		project, exists := registry.state.Projects[key.ProjectID]
		if !exists || project.Blocked || (project.TeamID != "" && key.TeamID != "" && project.TeamID != key.TeamID) {
			return nil, permissions, false
		}
		if key.TeamID == "" {
			key.TeamID = project.TeamID
		}
		permissions.project = project.ObjectPermission
		projectModels = unrestrictedScope(project.Models)
	}
	scope := unrestrictedScope(key.Models)
	if key.TeamID != "" && slices.Contains(scope, "all-team-models") {
		scope = nil
	}
	scope = intersectScopes(scope, projectModels)
	if key.UserID != "" {
		user, exists := registry.state.Users[key.UserID]
		if !exists || user.Role != "internal_user" {
			return nil, permissions, false
		}
		scope = intersectScopes(scope, unrestrictedScope(user.Models))
	}
	orgID := key.OrgID
	if key.TeamID != "" {
		owner, exists := registry.state.Teams[key.TeamID]
		if !exists || owner.Blocked || (orgID != "" && orgID != owner.OrgID) {
			return nil, permissions, false
		}
		scope = intersectScopes(scope, unrestrictedScope(owner.Models))
		permissions.team = owner.ObjectPermission
		index := memberIndex(owner, key.UserID)
		if session, _ := key.Metadata["_minato_cli_session"].(bool); session &&
			(index < 0 || owner.Members[index].Role == "internal_user_viewer") {
			return nil, permissions, false
		}
		if index >= 0 {
			scope = intersectScopes(scope, unrestrictedScope(owner.Members[index].Models))
		}
		orgID = owner.OrgID
	}
	if orgID != "" {
		owner, exists := registry.state.Organizations[orgID]
		if !exists {
			return nil, permissions, false
		}
		if index := organizationMemberIndex(owner, key.UserID); index >= 0 &&
			owner.Members[index].Role == "internal_user_viewer" {
			return nil, permissions, false
		}
		scope = intersectScopes(scope, unrestrictedScope(owner.Models))
	}
	return scope, permissions, true
}

func intersectScopes(key, team []string) []string {
	switch {
	case key == nil:
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
	mux.HandleFunc("POST /key/regenerate", registry.regenerateKey)
	mux.HandleFunc("POST /key/block", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, true)
	})
	mux.HandleFunc("POST /key/unblock", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, false)
	})
	mux.HandleFunc("GET /key/list", registry.listKeys)
	registry.registerTeamRoutes(mux)
	registry.registerOrganizationRoutes(mux)
	registry.registerUserRoutes(mux)
	registry.registerProjectRoutes(mux)
	registry.registerUISSO(mux)
	mux.Handle("/", next)
	return mux
}

func (registry *KeyRegistry) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(registry.master), []byte(runtime.ProxyCredential(r))) == 1 {
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
		ProjectID string        `json:"project_id"`
		OrgID    string         `json:"organization_id"`
		UserID   string         `json:"user_id"`
		Blocked  bool           `json:"blocked"`
		Metadata map[string]any `json:"metadata"`
		ObjectPermission *objectPermission `json:"object_permission"`
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
	key := virtualKey{Alias: input.Alias, Models: append([]string{}, input.Models...), TeamID: input.TeamID, ProjectID: input.ProjectID, OrgID: input.OrgID, UserID: input.UserID,
		Blocked: input.Blocked, Metadata: input.Metadata, ObjectPermission: input.ObjectPermission, Expires: expiry, CreatedAt: &now}
	token := newKeyToken()
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
		Aliases []string `json:"key_aliases"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		len(input.Keys)+len(input.Aliases) == 0 || slices.Contains(input.Aliases, "") {
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
	if len(input.Aliases) != 0 {
		for digest, key := range next.Keys {
			if slices.Contains(input.Aliases, key.Alias) {
				delete(next.Keys, digest)
				removed++
			}
		}
	}
	if removed > 0 {
		for digest, key := range next.Keys {
			if _, exists := next.Keys[key.RotationTarget]; key.RotationTarget != "" && !exists {
				delete(next.Keys, digest)
			}
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

func newKeyToken() string {
	var random [32]byte
	rand.Read(random[:])
	return "sk-" + hex.EncodeToString(random[:])
}
