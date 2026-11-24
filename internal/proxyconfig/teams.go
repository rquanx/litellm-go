package proxyconfig

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

type team struct {
	MemberPermissions *[]string `json:"team_member_permissions,omitempty"`
	AccessGroupIDs []string `json:"access_group_ids,omitempty"`
	ownerRates
	ID       string         `json:"team_id"`
	Alias    string         `json:"team_alias"`
	OrgID    string         `json:"organization_id,omitempty"`
	Models   []string       `json:"models"`
	Blocked  bool           `json:"blocked"`
	Metadata map[string]any `json:"metadata"`
	Callbacks json.RawMessage `json:"callbacks,omitempty"`
	RouterSettings map[string]any `json:"router_settings,omitempty"`
	ObjectPermission *objectPermission `json:"object_permission,omitempty"`
	Members []teamMembership `json:"members_with_roles,omitempty"`
	MaxBudget *float64 `json:"max_budget,omitempty"`
	BudgetDuration string `json:"budget_duration,omitempty"`
	BudgetResetAt *time.Time `json:"budget_reset_at,omitempty"`
	BudgetLimits []keyBudgetLimit `json:"budget_limits,omitempty"`
	ImportedSpend *importedKeySpend `json:"imported_spend,omitempty"`
	MemberBudget *float64 `json:"team_member_budget,omitempty"`
	MemberBudgetDuration string `json:"team_member_budget_duration,omitempty"`
	MemberBudgetResetAt *time.Time `json:"team_member_budget_reset_at,omitempty"`
}

type teamChanges struct {
	MemberPermissions *[]string `json:"team_member_permissions"`
	AccessGroupIDs *[]string `json:"access_group_ids"`
	ownerRateChanges
	ID       string          `json:"team_id"`
	Alias    *string         `json:"team_alias"`
	OrgID    *string         `json:"organization_id"`
	Models   *[]string       `json:"models"`
	Blocked  *bool           `json:"blocked"`
	Metadata *map[string]any `json:"metadata"`
	AllowedPassThroughRoutes *[]string `json:"allowed_passthrough_routes"`
	RouterSettings json.RawMessage `json:"router_settings"`
	ObjectPermission json.RawMessage `json:"object_permission"`
	MaxBudget json.RawMessage `json:"max_budget"`
	BudgetDuration json.RawMessage `json:"budget_duration"`
	BudgetLimits json.RawMessage `json:"budget_limits"`
	MemberBudget json.RawMessage `json:"team_member_budget"`
	MemberBudgetDuration json.RawMessage `json:"team_member_budget_duration"`
}

func (entry team) public() team {
	entry.ImportedSpend = nil
	entry.Callbacks = nil
	entry.BudgetLimits = publicBudgetLimits(entry.BudgetLimits)
	entry.Members = slices.Clone(entry.Members)
	for i := range entry.Members {
		entry.Members[i].ImportedSpend = nil
	}
	return entry
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
	mux.HandleFunc("GET /team/available", registry.availableTeams)
	mux.HandleFunc("GET /team/permissions_list", registry.teamPermissions)
	mux.HandleFunc("POST /team/permissions_update", registry.teamPermissions)
	mux.HandleFunc("POST /team/permissions_bulk_update", registry.bulkTeamPermissions)
	mux.HandleFunc("GET /team/{team_id}/members/me", registry.teamMemberMe)
	mux.HandleFunc("GET /team/{team_id}/callback", registry.teamCallback)
	mux.HandleFunc("POST /team/{team_id}/callback", registry.teamCallback)
	mux.HandleFunc("POST /team/{team_id}/disable_logging", registry.disableTeamLogging)
	mux.HandleFunc("GET /team/list", registry.listTeams)
	mux.HandleFunc("GET /v2/team/list", registry.listTeams)
	mux.HandleFunc("POST /team/delete", registry.deleteTeams)
	mux.HandleFunc("POST /team/model/add", registry.changeTeamModels)
	mux.HandleFunc("POST /team/model/delete", registry.changeTeamModels)
	mux.HandleFunc("POST /team/bulk_member_add", func(w http.ResponseWriter, r *http.Request) {
		registry.writeTeamMembership(w, r, "add")
	})
	for _, operation := range []string{"add", "update", "delete"} {
		mux.HandleFunc("POST /team/member_"+operation, func(w http.ResponseWriter, r *http.Request) {
			registry.writeTeamMembership(w, r, operation)
		})
	}
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
	if input.Models != nil && !validOwnerModels(*input.Models) {
		modelError(w, http.StatusBadRequest, "Invalid team model scope")
		return
	}
	if input.ID == "" {
		if update {
			modelError(w, http.StatusBadRequest, "Supply a team ID")
			return
		}
		input.ID = newEntityID()
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
		registry.ssoTeamDefaultsLocked().applyTo(&input)
	}
	if input.Alias != nil {
		current.Alias = *input.Alias
	}
	if input.OrgID != nil {
		if *input.OrgID != "" {
			if _, exists := registry.state.Organizations[*input.OrgID]; !exists {
				modelError(w, http.StatusBadRequest, "Organization not found")
				return
			}
		}
		for _, key := range registry.state.Keys {
			if key.TeamID == current.ID && key.OrgID != "" && key.OrgID != *input.OrgID {
				modelError(w, http.StatusConflict, "Team has keys bound to its organization")
				return
			}
		}
		current.OrgID = *input.OrgID
	}
	if input.Models != nil {
		current.Models = *input.Models
	}
	if input.MemberPermissions != nil {
		if !validTeamPermissions(*input.MemberPermissions) {
			modelError(w, http.StatusBadRequest, "Invalid team member permissions")
			return
		}
		current.MemberPermissions = input.MemberPermissions
	}
	if input.AccessGroupIDs != nil {
		if !registry.state.validAccessGroupIDs(*input.AccessGroupIDs) {
			modelError(w, http.StatusBadRequest, "Unknown access group ID")
			return
		}
		current.AccessGroupIDs = *input.AccessGroupIDs
	}
	if input.ObjectPermission != nil {
		var permission *objectPermission
		if err := decodeModelJSON(strings.NewReader(string(input.ObjectPermission)), &permission); err != nil {
			modelError(w, http.StatusBadRequest, "Invalid or unsupported object permission")
			return
		}
		current.ObjectPermission = permission
	}
	if input.Blocked != nil {
		current.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		current.Metadata = *input.Metadata
	}
	if input.AllowedPassThroughRoutes != nil {
		current.Metadata = withPassThroughRoutes(current.Metadata, *input.AllowedPassThroughRoutes)
	}
	if err := applyRouterSettings(input.RouterSettings, &current.RouterSettings); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := registry.updateBudget(input.MaxBudget, input.BudgetDuration, &current.MaxBudget, &current.BudgetDuration, &current.BudgetResetAt); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := input.ownerRateChanges.apply(&current.ownerRates); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := registry.updateBudget(input.MemberBudget, input.MemberBudgetDuration, &current.MemberBudget,
		&current.MemberBudgetDuration, &current.MemberBudgetResetAt); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.BudgetLimits != nil {
		var limits []keyBudgetLimit
		if decodeModelJSON(strings.NewReader(string(input.BudgetLimits)), &limits) != nil {
			modelError(w, http.StatusBadRequest, "Invalid budget_limits")
			return
		}
		if err := registry.setBudgetLimits(&current.BudgetLimits, limits, time.Now()); err != nil {
			modelError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	registry.saveTeam(w, r, current)
}

func newEntityID() string {
	var id [16]byte
	rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func validOwnerModels(models []string) bool {
	return !slices.ContainsFunc(models, func(model string) bool {
		return strings.TrimSpace(model) == "" || model == "all-team-models"
	})
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
	registry.saveTeam(w, r, current)
}

func (registry *KeyRegistry) saveTeam(w http.ResponseWriter, r *http.Request, current team) {
	next := registry.state
	next.Teams = maps.Clone(next.Teams)
	next.Teams[current.ID] = current
	var before any
	action := "created"
	if old, exists := registry.state.Teams[current.ID]; exists {
		action = "updated"
		before = auditTeamSnapshot(old)
	}
	if registry.apply(w, next, registry.entityAudit(r, "LiteLLM_TeamTable", current.ID, action, before, auditTeamSnapshot(current))) {
		modelJSON(w, http.StatusOK, current.public())
	}
}

func (registry *KeyRegistry) changeTeamModels(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID     string   `json:"team_id"`
		Models []string `json:"models"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		input.ID == "" || input.Models == nil || !validOwnerModels(input.Models) {
		modelError(w, http.StatusBadRequest, "Supply a team ID and a valid model list")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[input.ID]
	if !registry.requireTeamAdminLocked(w, r, current) {
		return
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	models := append([]string{}, current.Models...)
	if r.URL.Path == "/team/model/add" {
		if len(models) == 0 {
			models = append(models, "all-proxy-models")
		}
		seen := make(map[string]bool, len(models)+len(input.Models))
		models = slices.DeleteFunc(append(models, input.Models...), func(model string) bool {
			duplicate := seen[model]
			seen[model] = true
			return duplicate
		})
	} else {
		remove := make(map[string]bool, len(input.Models))
		for _, model := range input.Models {
			remove[model] = true
		}
		models = slices.DeleteFunc(models, func(model string) bool { return remove[model] })
	}
	current.Models = models
	registry.saveTeam(w, r, current)
}

func (registry *KeyRegistry) teamInfo(w http.ResponseWriter, r *http.Request) {
	registry.mu.RLock()
	caller, ok := registry.identifyReadCallerLocked(r)
	if !ok || (!caller.admin && caller.user == "" && caller.team == "") {
		registry.mu.RUnlock()
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	id := r.URL.Query().Get("team_id")
	if id == "" {
		registry.mu.RUnlock()
		modelError(w, http.StatusUnprocessableEntity, "Supply a team ID")
		return
	}
	current, exists := registry.state.Teams[id]
	if !caller.admin && caller.team != id && memberIndex(current, caller.user) < 0 &&
		organizationRole(registry.state.Organizations[current.OrgID], caller.user) != "org_admin" {
		registry.mu.RUnlock()
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	keys := make([]virtualKey, 0)
	now := time.Now()
	for _, key := range registry.state.Keys {
		if key.TeamID == id && (caller.admin || (caller.user != "" && key.UserID == caller.user)) &&
			(key.Expires == nil || now.Before(*key.Expires)) {
			key.ImportedSpend = nil
			key.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
			keys = append(keys, key)
		}
	}
	registry.mu.RUnlock()
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	start, reset, spend := registry.spendWindow(current.BudgetDuration, current.BudgetResetAt, current.ImportedSpend, now)
	current.BudgetResetAt = reset
	current = current.public()
	current.BudgetLimits = slices.Clone(current.BudgetLimits)
	for i, limit := range current.BudgetLimits {
		_, current.BudgetLimits[i].ResetAt = registry.budgetWindow(limit.Duration, limit.ResetAt, now)
	}
	if registry.teamSpend != nil {
		recorded, err := registry.teamSpend(r.Context(), id, start)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read team spend")
			return
		}
		spend += recorded
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"team_id": id, "team_info": struct {
			team
			Spend float64 `json:"spend"`
		}{current, spend}, "keys": keys, "team_memberships": append([]teamMembership{}, current.Members...),
	})
}

// teamCallback exposes the LiteLLM team callback configuration surface. The
// callback schema is provider-specific, so the proxy deliberately preserves
// the JSON object instead of restricting it to a fixed set of fields.
func (registry *KeyRegistry) teamCallback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("team_id")
	if id == "" {
		modelError(w, http.StatusBadRequest, "Supply a team ID")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[id]
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	if !registry.requireTeamAdminLocked(w, r, current) {
		return
	}
	if current.Metadata == nil {
		current.Metadata = map[string]any{}
	}
	if r.Method == http.MethodGet {
		modelJSON(w, http.StatusOK, map[string]any{"status": "success", "data": teamCallbackView(id, current.Metadata)})
		return
	}
	var input struct {
		CallbackName string         `json:"callback_name"`
		CallbackType string         `json:"callback_type"`
		CallbackVars map[string]any `json:"callback_vars"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.CallbackName == "" ||
		(input.CallbackType != "success" && input.CallbackType != "failure" && input.CallbackType != "success_and_failure") {
		modelError(w, http.StatusBadRequest, "callback_name, callback_type and callback_vars are required")
		return
	}
	entries, _ := current.Metadata["logging"].([]any)
	for _, raw := range entries {
		if entry, ok := raw.(map[string]any); ok && entry["callback_name"] == input.CallbackName && entry["callback_type"] == input.CallbackType {
			modelError(w, http.StatusBadRequest, "Callback already exists for this team and event")
			return
		}
	}
	entries = append(entries, map[string]any{"callback_name": input.CallbackName, "callback_type": input.CallbackType, "callback_vars": input.CallbackVars})
	current.Metadata["logging"] = entries
	next := registry.state
	next.Teams = maps.Clone(registry.state.Teams)
	next.Teams[id] = current
	if !registry.apply(w, next) {
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "data": current.public()})
}

func teamCallbackView(id string, metadata map[string]any) map[string]any {
	result := map[string]any{"team_id": id, "success_callbacks": []any{}, "failure_callbacks": []any{}, "callback_vars": map[string]any{}}
	if settings, ok := metadata["callback_settings"].(map[string]any); ok {
		for _, key := range []string{"success_callback", "success_callbacks", "failure_callback", "failure_callbacks", "callback_vars"} {
			if value, exists := settings[key]; exists {
				result[key] = value
			}
		}
	}
	if entries, ok := metadata["logging"].([]any); ok {
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := entry["callback_name"].(string)
			typ, _ := entry["callback_type"].(string)
			if typ == "success" || typ == "success_and_failure" {
				result["success_callbacks"] = append(result["success_callbacks"].([]any), name)
			}
			if typ == "failure" || typ == "success_and_failure" {
				result["failure_callbacks"] = append(result["failure_callbacks"].([]any), name)
			}
			if vars, exists := entry["callback_vars"]; exists {
				result["callback_vars"] = vars
			}
		}
	}
	return result
}

func (registry *KeyRegistry) disableTeamLogging(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("team_id")
	if id == "" {
		modelError(w, http.StatusBadRequest, "Supply a team ID")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	current, exists := registry.state.Teams[id]
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}
	if !registry.requireTeamAdminLocked(w, r, current) {
		return
	}
	metadata := maps.Clone(current.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	delete(metadata, "logging")
	metadata["callback_settings"] = map[string]any{"success_callback": []any{}, "failure_callback": []any{}, "callback_vars": map[string]any{}}
	current.Metadata = metadata
	next := registry.state
	next.Teams = maps.Clone(registry.state.Teams)
	next.Teams[id] = current
	if !registry.apply(w, next) {
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Logging disabled for team " + id,
		"data": map[string]any{"team_id": id, "success_callbacks": []any{}, "failure_callbacks": []any{}}})
}

func (registry *KeyRegistry) availableTeams(w http.ResponseWriter, r *http.Request) {
	registry.mu.RLock()
	caller, valid := registry.identifyReadCallerLocked(r)
	if !valid {
		registry.mu.RUnlock()
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	teams := []team{}
	if defaults := registry.ssoUserDefaultsLocked(); defaults != nil && len(defaults.AvailableTeams) > 0 {
		if _, exists := registry.state.Users[caller.user]; !exists && !caller.admin {
			registry.mu.RUnlock()
			modelError(w, http.StatusNotFound, "User not found")
			return
		}
		seen := make(map[string]bool)
		for _, id := range defaults.AvailableTeams {
			if entry, exists := registry.state.Teams[id]; exists && !seen[id] && memberIndex(entry, caller.user) < 0 {
				teams = append(teams, entry.public())
				seen[id] = true
			}
		}
	}
	registry.mu.RUnlock()
	modelJSON(w, http.StatusOK, teams)
}

func (registry *KeyRegistry) listTeams(w http.ResponseWriter, r *http.Request) {
	caller, ok := registry.identifyReadCaller(r)
	if !ok || (!caller.admin && caller.user == "") {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	query := r.URL.Query()
	orgID, userID := query.Get("organization_id"), query.Get("user_id")
	paginated := r.URL.Path == "/v2/team/list"
	var page, size int
	var id, alias, search string
	if paginated {
		var pageErr, sizeErr error
		page, pageErr = keyListInt(query, "page", 1)
		size, sizeErr = keyListInt(query, "page_size", 10)
		if pageErr != nil || sizeErr != nil || size > 100 {
			modelError(w, http.StatusUnprocessableEntity, "Invalid page or page_size")
			return
		}
		if query.Get("status") != "" {
			modelError(w, http.StatusBadRequest, "Deleted team history is unavailable")
			return
		}
		id, alias, search = query.Get("team_id"), strings.ToLower(query.Get("team_alias")), strings.ToLower(query.Get("search"))
	}
	registry.mu.RLock()
	adminOrgs := make(map[string]bool)
	if !caller.admin && paginated {
		for id, org := range registry.state.Organizations {
			if organizationRole(org, caller.user) == "org_admin" {
				adminOrgs[id] = true
			}
		}
	}
	if !caller.admin {
		if len(adminOrgs) > 0 {
			if orgID != "" && !adminOrgs[orgID] {
				registry.mu.RUnlock()
				modelError(w, http.StatusForbidden, "You can only view teams within your organizations")
				return
			}
		} else {
			if userID != "" && userID != caller.user {
				registry.mu.RUnlock()
				modelError(w, http.StatusForbidden, "Team list is outside the key's user scope")
				return
			}
			userID = caller.user
		}
	}
	teams := make([]team, 0, len(registry.state.Teams))
	for _, id := range slices.Sorted(maps.Keys(registry.state.Teams)) {
		current := registry.state.Teams[id]
		orgMatches := orgID == "" || orgID == current.OrgID || (orgID == "default_organization" && current.OrgID == "")
		if orgMatches && (len(adminOrgs) == 0 || adminOrgs[current.OrgID]) &&
			(userID == "" || memberIndex(current, userID) >= 0) {
			teams = append(teams, current.public())
		}
	}
	registry.mu.RUnlock()
	if paginated {
		teams = slices.DeleteFunc(teams, func(entry team) bool {
			lowerAlias := strings.ToLower(entry.Alias)
			return (id != "" && entry.ID != id) || !strings.Contains(lowerAlias, alias) ||
				(search != "" && strings.ToLower(entry.ID) != search && !strings.Contains(lowerAlias, search))
		})
		column := query.Get("sort_by")
		if column == "team_alias" {
			slices.SortStableFunc(teams, func(a, b team) int { return strings.Compare(a.Alias, b.Alias) })
		}
		if strings.EqualFold(query.Get("sort_order"), "desc") {
			slices.Reverse(teams)
		}
		count := len(teams)
		pages := (count + size - 1) / size
		if page > pages {
			teams = teams[:0]
		} else {
			start := (page - 1) * size
			teams = teams[start:min(count, start+size)]
		}
		modelJSON(w, http.StatusOK, map[string]any{
			"teams": teams, "total": count, "page": page, "page_size": size, "total_pages": pages,
		})
		return
	}
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
	next.Projects = maps.Clone(next.Projects)
	count := 0
	var events []*managementAuditEvent
	for _, id := range input.IDs {
		if id == "" {
			modelError(w, http.StatusBadRequest, "Team IDs must be nonempty")
			return
		}
		if entry, exists := next.Teams[id]; exists {
			if registry.storeAuditLogs {
				events = append(events, registry.entityAudit(r, "LiteLLM_TeamTable", id, "deleted", auditTeamSnapshot(entry), nil))
			}
			delete(next.Teams, id)
			count++
		}
	}
	next.pruneAccessGroupTeamOverrides()
	for digest, key := range next.Keys {
		if slices.Contains(input.IDs, key.TeamID) || slices.Contains(input.IDs, next.Projects[key.ProjectID].TeamID) {
			delete(next.Keys, digest)
		}
	}
	for id, project := range next.Projects {
		if slices.Contains(input.IDs, project.TeamID) {
			delete(next.Projects, id)
		}
	}
	next.archiveRevokedKeys(registry.state, "default_user_id")
	if registry.apply(w, next, events...) {
		modelJSON(w, http.StatusOK, map[string]int{"count": count})
	}
}

func auditTeamSnapshot(entry team) map[string]any {
	return map[string]any{"team_id": entry.ID, "team_alias": entry.Alias, "organization_id": entry.OrgID,
		"models": entry.Models, "blocked": entry.Blocked, "max_budget": entry.MaxBudget, "budget_duration": entry.BudgetDuration}
}
