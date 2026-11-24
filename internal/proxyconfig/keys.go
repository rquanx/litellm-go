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
	"sync/atomic"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type virtualKey struct {
	AccessGroupIDs []string `json:"access_group_ids,omitempty"`
	RestrictedGroupIDs []string `json:"restricted_access_group_ids,omitempty"`
	Alias     string         `json:"key_alias,omitempty"`
	Models    []string       `json:"models"`
	Expires   *time.Time     `json:"expires,omitempty"`
	TeamID    string         `json:"team_id,omitempty"`
	ProjectID string         `json:"project_id,omitempty"`
	OrgID     string         `json:"organization_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	Blocked   bool           `json:"blocked"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	RouterSettings map[string]any `json:"router_settings,omitempty"`
	ObjectPermission *objectPermission `json:"object_permission,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	CreatedBy string        `json:"created_by,omitempty"`
	RotationTarget string     `json:"rotation_target,omitempty"`
	MaxBudget *float64 `json:"max_budget,omitempty"`
	BudgetDuration string `json:"budget_duration,omitempty"`
	BudgetResetAt *time.Time `json:"budget_reset_at,omitempty"`
	BudgetLimits []keyBudgetLimit `json:"budget_limits,omitempty"`
	ModelBudgets map[string]keyModelBudget `json:"model_max_budget,omitempty"`
	SpendIdentity string `json:"spend_identity,omitempty"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
	BudgetID string `json:"budget_id,omitempty"`
	RPMLimit *int `json:"rpm_limit,omitempty"`
	TPMLimit *int `json:"tpm_limit,omitempty"`
	ParallelLimit *int `json:"max_parallel_requests,omitempty"`
}

type accessState struct {
	SSOTeamDefaults *ssoTeamDefaults `json:"sso_team_defaults,omitempty"`
	SSOUserDefaults *SSOUserDefaults `json:"sso_user_defaults,omitempty"`
	DeletedKeys map[string]deletedVirtualKey `json:"deleted_keys,omitempty"`
	AccessGroups map[string]accessGroup `json:"access_groups,omitempty"`
	AccessGroupTeamOverrides map[string][]string `json:"access_group_team_overrides,omitempty"`
	Keys  map[string]virtualKey `json:"keys"`
	Teams map[string]team       `json:"teams"`
	Organizations map[string]organization `json:"organizations,omitempty"`
	Users map[string]internalUser `json:"users,omitempty"`
	Passwords map[string]string `json:"password_hashes,omitempty"`
	Invitations map[string]dashboardInvitation `json:"invitations,omitempty"`
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
	storeAuditLogs bool
	modelAccessGroups atomic.Pointer[map[string][]string]
	passwordSlots chan struct{}
	master string
	store  stateStore
	mu     sync.RWMutex
	state  accessState
	ssoDefaults *SSOUserDefaults
	ssoTeamDefaults *ssoTeamDefaults
	ssoTeamClaim string
	browserSSO *postgresBrowserSSO
	teamMemberSpend func(context.Context, string, string, time.Time) (float64, error)
	userSpend func(context.Context, string, time.Time) (float64, error)
	teamSpend func(context.Context, string, time.Time) (float64, error)
	budgetMaximum func(context.Context, string) (*float64, error)
	budgetPeriod func(context.Context, string) (budgetPeriod, error)
	keySpend func(context.Context, string, time.Time) (float64, error)
	budgetZone *time.Location
}

func LoadKeys(master, path string) (*KeyRegistry, error) {
	return loadKeys(master, chooseStateStore(path, nil, "keys"))
}

func LoadPostgresKeys(master string, db *sql.DB) (*KeyRegistry, error) {
	registry, err := loadKeys(master, chooseStateStore("", db, "keys"))
	if err != nil {
		return nil, err
	}
	ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
	defer cancel()
	if _,err:=db.ExecContext(ctx,`CREATE TABLE IF NOT EXISTS `+ssoConfigTable+` (
		id TEXT PRIMARY KEY, sso_settings JSONB NOT NULL,
		created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
	)`);err!=nil {
		return nil,err
	}
	registry.browserSSO, err = openPostgresBrowserSSO(db, master)
	if err != nil {
		return nil, err
	}
	return registry, nil
}

func loadKeys(master string, store stateStore) (*KeyRegistry, error) {
	if master == "" {
		return nil, errors.New("virtual keys require a proxy master key")
	}
	registry := &KeyRegistry{master: master, store: store, state: emptyAccessState(), passwordSlots: make(chan struct{}, 2)}
	if store == nil {
		return registry, nil
	}
	if err := store.load(&registry.state); err != nil || registry.state.Keys == nil || registry.state.Teams == nil {
		return nil, errors.New("invalid virtual key state")
	}
	return registry, nil
}

func emptyAccessState() accessState {
	return accessState{Keys: make(map[string]virtualKey), Teams: make(map[string]team), Organizations: make(map[string]organization)}
}

func (registry *KeyRegistry) RunRefresh(ctx context.Context) {
	if _, ok := registry.store.(*postgresState); !ok {
		return
	}
	refreshPostgresRegistry(ctx, "virtual keys and access scopes", registry.refresh)
}

func (registry *KeyRegistry) refresh(ctx context.Context) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	store := registry.store.(*postgresState)
	next := *store
	state := emptyAccessState()
	changed, err := next.loadAfter(ctx, &state, store.revision)
	if err != nil || !changed {
		return err
	}
	if state.Keys == nil || state.Teams == nil {
		return errors.New("invalid virtual key state")
	}
	registry.state = state
	store.revision = next.revision
	return nil
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
	BlockedTools []string `json:"blocked_tools,omitempty"`
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
	if runtime.MCPToolMatches(permission.BlockedTools, tool) {
		return false
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
	keyGrants := registry.state.groupGrants(key.AccessGroupIDs)
	permissions.key = addGroupObjectPermissions(key.ObjectPermission, keyGrants)
	if readOnly,_:=key.Metadata["_minato_sso_view_only"].(bool);readOnly {
		return nil,permissions,false
	}
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
	scope := registry.modelScope(key.Models)
	if key.TeamID != "" && slices.Contains(scope, "all-team-models") {
		scope = nil
	}
	scope = addGroupModels(scope, keyGrants.Models)
	scope = intersectScopes(scope, projectModels)
	if key.UserID != "" {
		user, exists := registry.state.Users[key.UserID]
		if !exists || user.Role != "internal_user" {
			return nil, permissions, false
		}
		if user.DefaultOrgID != "" {
			if (key.OrgID != "" && key.OrgID != user.DefaultOrgID) ||
				organizationRole(registry.state.Organizations[user.DefaultOrgID], user.ID) == "" {
				return nil, permissions, false
			}
			key.OrgID = user.DefaultOrgID
		}
		scope = intersectScopes(scope, unrestrictedScope(user.Models))
	}
	orgID := key.OrgID
	if key.TeamID != "" {
		owner, exists := registry.state.Teams[key.TeamID]
		if !exists || owner.Blocked || (orgID != "" && orgID != owner.OrgID) {
			return nil, permissions, false
		}
		teamGrants := registry.state.groupGrants(owner.AccessGroupIDs)
		teamModels := addGroupModels(registry.modelScope(owner.Models), teamGrants.Models)
		keyTeamGrants := keyGrants.Models
		if len(key.RestrictedGroupIDs) > 0 {
			keyTeamGrants = registry.state.keyGroupTeamModels(key)
		}
		teamModels = addGroupModels(teamModels, keyTeamGrants)
		scope = intersectScopes(scope, teamModels)
		permissions.team = addGroupObjectPermissions(owner.ObjectPermission, teamGrants)
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
	if len(models) == 0 || slices.Contains(models, "*") || slices.Contains(models, "all-proxy-models") {
		return nil
	}
	return models
}

func (registry *KeyRegistry) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /key/generate", registry.generate)
	mux.HandleFunc("POST /key/service-account/generate", registry.generate)
	mux.HandleFunc("GET /key/info", registry.info)
	mux.HandleFunc("POST /v2/key/info", registry.keyInfoBatch)
	mux.HandleFunc("GET /key/aliases", registry.keyAliases)
	mux.HandleFunc("POST /key/delete", registry.remove)
	mux.HandleFunc("POST /key/update", registry.updateKey)
	mux.HandleFunc("POST /key/regenerate", registry.regenerateKey)
	mux.HandleFunc("POST /key/{key}/regenerate", registry.regenerateKey)
	mux.HandleFunc("POST /key/{key}/reset_spend", registry.resetKeySpend)
	mux.HandleFunc("POST /key/bulk_update", registry.bulkUpdateKeys)
	mux.HandleFunc("POST /team/key/bulk_update", registry.bulkUpdateTeamKeys)
	mux.HandleFunc("POST /key/block", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, true)
	})
	mux.HandleFunc("POST /key/unblock", func(w http.ResponseWriter, r *http.Request) {
		registry.setKeyBlocked(w, r, false)
	})
	mux.HandleFunc("GET /key/list", registry.listKeys)
	registry.registerTeamRoutes(mux)
	registry.registerOrganizationRoutes(mux)
	registry.registerAccessGroupRoutes(mux)
	registry.registerUserRoutes(mux)
	registry.registerSCIMRoutes(mux)
	registry.registerSSOProvisionSettings(mux)
	registry.registerDashboardInvitationRoutes(mux)
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
		Key string `json:"key"`
		Alias    string         `json:"key_alias"`
		AccessGroupIDs []string `json:"access_group_ids"`
		Duration string         `json:"duration"`
		Models   []string       `json:"models"`
		TeamID   string         `json:"team_id"`
		ProjectID string        `json:"project_id"`
		OrgID    string         `json:"organization_id"`
		UserID   string         `json:"user_id"`
		Blocked  bool           `json:"blocked"`
		Metadata map[string]any `json:"metadata"`
		AllowedPassThroughRoutes *[]string `json:"allowed_passthrough_routes"`
		RouterSettings map[string]any `json:"router_settings"`
		ObjectPermission *objectPermission `json:"object_permission"`
		MaxBudget *float64 `json:"max_budget"`
		BudgetDuration string `json:"budget_duration"`
		BudgetLimits []keyBudgetLimit `json:"budget_limits"`
		ModelBudgets map[string]keyModelBudget `json:"model_max_budget"`
		BudgetID json.RawMessage `json:"budget_id"`
		RPMLimit *int `json:"rpm_limit"`
		TPMLimit *int `json:"tpm_limit"`
		ParallelLimit *int `json:"max_parallel_requests"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key configuration")
		return
	}
	if r.URL.Path == "/key/service-account/generate" {
		if input.TeamID == "" {
			modelError(w, http.StatusBadRequest, "team_id is required for service account keys")
			return
		}
		input.UserID = ""
	}
	now := time.Now().UTC()
	expiry, err := keyExpiry(now, input.Duration)
	if err != nil {
		modelError(w, http.StatusBadRequest, "Invalid key duration")
		return
	}
	key := virtualKey{Alias: input.Alias, Models: append([]string{}, input.Models...), TeamID: input.TeamID, ProjectID: input.ProjectID, OrgID: input.OrgID, UserID: input.UserID,
		Blocked: input.Blocked, Metadata: input.Metadata, ObjectPermission: input.ObjectPermission, Expires: expiry, CreatedAt: &now, MaxBudget: input.MaxBudget}
	if input.AllowedPassThroughRoutes != nil {
		key.Metadata = withPassThroughRoutes(key.Metadata, *input.AllowedPassThroughRoutes)
	}
	key.ModelBudgets = input.ModelBudgets
	key.CreatedBy = "default_user_id"
	key.AccessGroupIDs = input.AccessGroupIDs
	key.RouterSettings = input.RouterSettings
	key.RPMLimit = input.RPMLimit
	key.TPMLimit = input.TPMLimit
	key.ParallelLimit = input.ParallelLimit
	if !customKeyAllowed(w, r, input.Key) {
		return
	}
	token := input.Key
	if token == "" {
		token = newKeyToken()
	} else if !validCustomAPIKey(token, registry.master) {
		modelError(w, http.StatusBadRequest, "Invalid custom API key")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if input.BudgetID != nil {
		id, err := registry.validateBudgetID(r.Context(), input.BudgetID)
		if err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
		key.BudgetID = id
	}
	if err := registry.setKeyBudgetDuration(&key, input.BudgetDuration, now); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := registry.setBudgetLimits(&key.BudgetLimits, input.BudgetLimits, now); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := registry.validateKey(key); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	digest := keyDigest(token)
	if _, exists := next.Keys[digest]; exists {
		modelError(w, http.StatusConflict, "Key already exists")
		return
	}
	next.Keys[digest] = key
	if registry.apply(w, next, registry.keyAudit(r, digest, "created", nil, &key)) {
		modelJSON(w, http.StatusOK, map[string]any{"key": token, "token_id": digest, "expires": key.Expires, "key_alias": key.Alias})
	}
}

func (registry *KeyRegistry) info(w http.ResponseWriter, r *http.Request) {
	caller, valid := registry.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	token := r.URL.Query().Get("key")
	callerToken := runtime.ProxyCredential(r)
	if token == "" {
		token = callerToken
	}
	registry.mu.RLock()
	digest := registry.resolveKey(token)
	key, exists := registry.state.Keys[digest]
	allowed := exists && registry.canReadKeyLocked(caller, keyDigest(callerToken), digest, key)
	registry.mu.RUnlock()
	if !allowed {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	now := time.Now()
	start, reset, baseline, err := registry.keySpendWindowWithTier(r.Context(), key, now)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read linked key budget")
		return
	}
	spend := baseline
	key.BudgetResetAt = reset
	key.ImportedSpend = nil
	key.BudgetLimits = slices.Clone(key.BudgetLimits)
	for i, limit := range key.BudgetLimits {
		_, key.BudgetLimits[i].ResetAt = registry.budgetWindow(limit.Duration, limit.ResetAt, now)
		key.BudgetLimits[i].ImportedSpend = nil
	}
	if registry.keySpend != nil {
		identity := key.SpendIdentity
		if identity == "" {
			identity = digest
		}
		recorded, err := registry.keySpend(r.Context(), identity, start)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read key spend")
			return
		}
		spend += recorded
	}
	modelJSON(w, http.StatusOK, map[string]any{"key": digest, "info": struct {
		virtualKey
		Spend float64 `json:"spend"`
	}{key, spend}})
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
	next.archiveRevokedKeys(registry.state, "default_user_id")
	var events []*managementAuditEvent
	if registry.storeAuditLogs {
		for digest, key := range registry.state.Keys {
			if _, kept := next.Keys[digest]; !kept && key.RotationTarget == "" {
				events = append(events, registry.keyAudit(r, digest, "deleted", &key, nil))
			}
		}
	}
	if registry.apply(w, next, events...) {
		modelJSON(w, http.StatusOK, map[string]int{"deleted_keys": removed})
	}
}

func (registry *KeyRegistry) apply(w http.ResponseWriter, next accessState, events ...*managementAuditEvent) bool {
	if err := registry.commitState(next, events...); err != nil {
		stateWriteError(w, err)
		return false
	}
	return true
}

func (registry *KeyRegistry) commitState(next accessState, events ...*managementAuditEvent) error {
	if registry.store != nil {
		var err error
		if store, ok := registry.store.(*postgresState); ok {
			err = store.saveWithAudit(next, events...)
		} else {
			err = registry.store.save(next)
		}
		if err != nil {
			return err
		}
	}
	registry.state = next
	return nil
}

func (registry *KeyRegistry) resetImportedSpend() error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := registry.state
	next.Keys = maps.Clone(registry.state.Keys)
	for digest, key := range next.Keys {
		key.ImportedSpend = nil
		for index := range key.BudgetLimits {
			key.BudgetLimits[index].ImportedSpend = nil
		}
		next.Keys[digest] = key
	}
	next.Teams = maps.Clone(registry.state.Teams)
	for id, entry := range next.Teams {
		entry.ImportedSpend = nil
		for index := range entry.BudgetLimits {
			entry.BudgetLimits[index].ImportedSpend = nil
		}
		next.Teams[id] = entry
	}
	return registry.commitState(next)
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
