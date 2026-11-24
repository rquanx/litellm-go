package proxyconfig

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

func keyExpiry(now time.Time, duration string) (*time.Time, error) {
	if duration == "" || duration == "-1" {
		return nil, nil
	}
	if strings.HasSuffix(duration, "mo") {
		months, err := strconv.Atoi(strings.TrimSuffix(duration, "mo"))
		if err != nil || months < 0 || months > (9999-now.Year())*12+12-int(now.Month()) {
			return nil, errors.New("invalid key duration")
		}
		target := now.AddDate(0, months, 1-now.Day())
		lastDay := target.AddDate(0, 1, -1).Day()
		expires := target.AddDate(0, 0, min(now.Day(), lastDay)-1)
		return &expires, nil
	}
	if strings.HasSuffix(duration, "d") || strings.HasSuffix(duration, "w") {
		count, err := strconv.Atoi(duration[:len(duration)-1])
		if err != nil || count < 0 || count > 3000000 {
			return nil, errors.New("invalid key duration")
		}
		days := count
		if strings.HasSuffix(duration, "w") {
			days *= 7
		}
		expires := now.AddDate(0, 0, days)
		if expires.Year() > 9999 {
			return nil, errors.New("invalid key duration")
		}
		return &expires, nil
	}
	interval, err := time.ParseDuration(duration)
	if err != nil || interval < 0 {
		return nil, errors.New("invalid key duration")
	}
	expires := now.Add(interval)
	return &expires, nil
}

func (registry *KeyRegistry) resolveKey(reference string) string {
	if len(reference) == 64 {
		if _, err := hex.DecodeString(reference); err == nil {
			normalized := strings.ToLower(reference)
			if _, exists := registry.state.Keys[normalized]; exists {
				return normalized
			}
		}
	}
	return keyDigest(reference)
}

func (registry *KeyRegistry) validateKey(key virtualKey) error {
	if !registry.state.validAccessGroupIDs(key.AccessGroupIDs) {
		return errors.New("Unknown access group ID")
	}
	if err := validateKeyModelBudgets(key.ModelBudgets, registry.keySpend != nil); err != nil {
		return err
	}
	if key.RPMLimit != nil && *key.RPMLimit < 0 {
		return errors.New("rpm_limit must be non-negative")
	}
	if key.TPMLimit != nil && *key.TPMLimit < 0 {
		return errors.New("tpm_limit must be non-negative")
	}
	if key.ParallelLimit != nil && *key.ParallelLimit < 0 {
		return errors.New("max_parallel_requests must be non-negative")
	}
	if key.MaxBudget != nil && (*key.MaxBudget < 0 || registry.keySpend == nil) {
		return errors.New("max_budget must be non-negative and requires PostgreSQL spend tracking")
	}
	if key.ProjectID != "" {
		project, exists := registry.state.Projects[key.ProjectID]
		if !exists || (project.TeamID != "" && key.TeamID != "" && project.TeamID != key.TeamID) {
			return errors.New("Project not found or belongs to a different team")
		}
		if key.TeamID == "" {
			key.TeamID = project.TeamID
		}
	}
	if key.UserID != "" {
		if _, exists := registry.state.Users[key.UserID]; !exists {
			return errors.New("User not found")
		}
	}
	if key.OrgID != "" {
		if _, exists := registry.state.Organizations[key.OrgID]; !exists {
			return errors.New("Organization not found")
		}
	}
	if key.TeamID != "" {
		team, exists := registry.state.Teams[key.TeamID]
		if !exists {
			return errors.New("Team not found")
		}
		if key.OrgID != "" && team.OrgID != key.OrgID {
			return errors.New("Team belongs to a different organization")
		}
	} else if slices.Contains(key.Models, "all-team-models") {
		return errors.New("Team model scope requires a team ID")
	}
	return nil
}

type keyUpdate struct {
	AccessGroupIDs *[]string `json:"access_group_ids"`
	Key      string          `json:"key"`
	Alias    *string         `json:"key_alias"`
	Models   *[]string       `json:"models"`
	TeamID   *string         `json:"team_id"`
	ProjectID *string        `json:"project_id"`
	OrgID    *string         `json:"organization_id"`
	UserID   *string         `json:"user_id"`
	Blocked  *bool           `json:"blocked"`
	Metadata *map[string]any `json:"metadata"`
	AllowedPassThroughRoutes *[]string `json:"allowed_passthrough_routes"`
	RouterSettings json.RawMessage `json:"router_settings"`
	Duration json.RawMessage `json:"duration"`
	ObjectPermission json.RawMessage `json:"object_permission"`
	MaxBudget json.RawMessage `json:"max_budget"`
	BudgetDuration json.RawMessage `json:"budget_duration"`
	BudgetLimits json.RawMessage `json:"budget_limits"`
	ModelBudgets json.RawMessage `json:"model_max_budget"`
	BudgetID json.RawMessage `json:"budget_id"`
	RPMLimit json.RawMessage `json:"rpm_limit"`
	TPMLimit json.RawMessage `json:"tpm_limit"`
	ParallelLimit json.RawMessage `json:"max_parallel_requests"`
	Tags *[]string `json:"tags"`
}

func (registry *KeyRegistry) updateKey(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input keyUpdate
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.Key == "" {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key update")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	digest := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[digest]
	if !exists || key.RotationTarget != "" {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	if err := registry.applyKeyChanges(r.Context(), &key, input); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	registry.saveKey(w, r, digest, key, true)
}

func (registry *KeyRegistry) applyKeyChanges(ctx context.Context, key *virtualKey, input keyUpdate) error {
	if input.AccessGroupIDs != nil {
		key.AccessGroupIDs = *input.AccessGroupIDs
		key.RestrictedGroupIDs = nil
	}
	if input.Alias != nil {
		key.Alias = *input.Alias
	}
	if input.Models != nil {
		key.Models = *input.Models
	}
	if input.TeamID != nil {
		key.TeamID = *input.TeamID
	}
	if input.ProjectID != nil {
		key.ProjectID = *input.ProjectID
	}
	if input.OrgID != nil {
		key.OrgID = *input.OrgID
	}
	if input.UserID != nil {
		key.UserID = *input.UserID
	}
	if input.Blocked != nil {
		key.Blocked = *input.Blocked
	}
	if input.Metadata != nil {
		key.Metadata = *input.Metadata
	}
	if input.AllowedPassThroughRoutes != nil {
		key.Metadata = withPassThroughRoutes(key.Metadata, *input.AllowedPassThroughRoutes)
	}
	if input.Tags != nil {
		metadata := make(map[string]any, len(key.Metadata)+1)
		maps.Copy(metadata, key.Metadata)
		metadata["tags"] = *input.Tags
		key.Metadata = metadata
	}
	if err := applyRouterSettings(input.RouterSettings, &key.RouterSettings); err != nil {
		return err
	}
	if input.MaxBudget != nil {
		var budget *float64
		if json.Unmarshal(input.MaxBudget, &budget) != nil {
			return errors.New("Invalid max_budget")
		}
		key.MaxBudget = budget
	}
	if input.BudgetDuration != nil {
		var duration string
		if json.Unmarshal(input.BudgetDuration, &duration) != nil {
			return errors.New("Invalid budget_duration")
		}
		if err := registry.setKeyBudgetDuration(key, duration, time.Now()); err != nil {
			return err
		}
	}
	if input.ModelBudgets != nil {
		var budgets map[string]keyModelBudget
		if err := json.Unmarshal(input.ModelBudgets, &budgets); err != nil {
			return errors.New("Invalid model_max_budget")
		}
		key.ModelBudgets = budgets
	}
	if input.BudgetLimits != nil {
		var limits []keyBudgetLimit
		if decodeModelJSON(strings.NewReader(string(input.BudgetLimits)), &limits) != nil {
			return errors.New("Invalid budget_limits")
		}
		if err := registry.setBudgetLimits(&key.BudgetLimits, limits, time.Now()); err != nil {
			return err
		}
	}
	if input.BudgetID != nil {
		id, err := registry.validateBudgetID(ctx, input.BudgetID)
		if err != nil {
			return err
		}
		key.BudgetID = id
	}
	if input.RPMLimit != nil {
		var limit *int
		if json.Unmarshal(input.RPMLimit, &limit) != nil {
			return errors.New("Invalid rpm_limit")
		}
		key.RPMLimit = limit
	}
	if input.TPMLimit != nil {
		var limit *int
		if json.Unmarshal(input.TPMLimit, &limit) != nil {
			return errors.New("Invalid tpm_limit")
		}
		key.TPMLimit = limit
	}
	if input.ParallelLimit != nil {
		var limit *int
		if json.Unmarshal(input.ParallelLimit, &limit) != nil {
			return errors.New("Invalid max_parallel_requests")
		}
		key.ParallelLimit = limit
	}
	if input.ObjectPermission != nil {
		var permission *objectPermission
		if err := decodeModelJSON(strings.NewReader(string(input.ObjectPermission)), &permission); err != nil {
			return errors.New("Invalid or unsupported object permission")
		}
		key.ObjectPermission = permission
	}
	if input.Duration != nil {
		var duration string
		if err := json.Unmarshal(input.Duration, &duration); err != nil {
			return errors.New("Invalid key duration")
		}
		expires, err := keyExpiry(time.Now().UTC(), duration)
		if err != nil {
			return errors.New("Invalid key duration")
		}
		key.Expires = expires
	}
	return registry.validateKey(*key)
}

func (registry *KeyRegistry) setKeyBlocked(w http.ResponseWriter, r *http.Request, blocked bool) {
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.Key == "" {
		modelError(w, http.StatusBadRequest, "Supply a virtual key")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	caller, valid := registry.identifyResourceCallerLocked(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	digest := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[digest]
	route := "/key/unblock"
	if blocked {
		route = "/key/block"
	}
	if !exists || !registry.canChangeTeamKeyLocked(caller, registry.state.Teams[key.TeamID], route) {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	key.Blocked = blocked
	registry.saveKey(w, r, digest, key, false)
}

type keyRecord struct {
	Token string `json:"token"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	DeletedBy string `json:"deleted_by,omitempty"`
	User *internalUser `json:"user,omitempty"`
	Creator *keyRecordCreator `json:"created_by_user,omitempty"`
	virtualKey
}

type keyRecordCreator struct {
	ID string `json:"user_id"`
	Email string `json:"user_email"`
	Alias string `json:"user_alias"`
}

type deletedVirtualKey struct {
	virtualKey
	DeletedAt time.Time `json:"deleted_at"`
	DeletedBy string `json:"deleted_by"`
}

func (next *accessState) archiveRevokedKeys(previous accessState, actor string) {
	now := time.Now().UTC()
	copied := false
	for digest, key := range previous.Keys {
		if _, retained := next.Keys[digest]; retained || key.RotationTarget != "" ||
			strings.HasPrefix(key.Alias, "ui-password:") || strings.HasPrefix(key.Alias, "ui-sso:") {
			continue
		}
		if !copied {
			next.DeletedKeys = maps.Clone(next.DeletedKeys)
			if next.DeletedKeys == nil {
				next.DeletedKeys = make(map[string]deletedVirtualKey)
			}
			copied = true
		}
		next.DeletedKeys[digest] = deletedVirtualKey{virtualKey: key, DeletedAt: now, DeletedBy: actor}
	}
}

func (registry *KeyRegistry) saveKey(w http.ResponseWriter, r *http.Request, digest string, key virtualKey, update bool) {
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	next.Keys[digest] = key
	before := registry.state.Keys[digest]
	if registry.apply(w, next, registry.keyAudit(r, digest, "updated", &before, &key)) {
		key.ImportedSpend = nil
		key.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
		if update {
			modelJSON(w, http.StatusOK, struct {
				Key string `json:"key"`
				keyRecord
			}{Key: digest, keyRecord: keyRecord{Token: digest, virtualKey: key}})
			return
		}
		modelJSON(w, http.StatusOK, keyRecord{Token: digest, virtualKey: key})
	}
}

func keyListInt(query url.Values, name string, fallback int) (int, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(query.Get(name))
	if err != nil || value <= 0 {
		return 0, errors.New("invalid pagination parameter")
	}
	return value, nil
}

func (registry *KeyRegistry) listKeys(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	for field := range query {
		switch field {
		case "page", "size", "key_hash", "team_id", "organization_id", "user_id", "key_alias", "return_full_object",
			"sort_by", "sort_order", "include_team_keys", "include_created_by_keys", "project_id", "access_group_id", "status", "expand":
		default:
			modelError(w, http.StatusBadRequest, "Unsupported key list filter")
			return
		}
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "size", 10)
	full, fullErr := strconv.ParseBool(query.Get("return_full_object"))
	if !query.Has("return_full_object") {
		fullErr = nil
	}
	order, column := query.Get("sort_order"), query.Get("sort_by")
	includeTeams, teamErr := optionalKeyListBool(query, "include_team_keys")
	includeCreated, createdErr := optionalKeyListBool(query, "include_created_by_keys")
	status := query.Get("status")
	expandUser := slices.Contains(query["expand"], "user")
	full = full || expandUser
	if pageErr != nil || sizeErr != nil || fullErr != nil || teamErr != nil || createdErr != nil || size > 100 ||
		(status != "" && status != "deleted") ||
		(order != "" && order != "asc" && order != "desc") ||
		(column != "" && column != "created_at" && column != "key_alias" && column != "user_id") {
		modelError(w, http.StatusBadRequest, "Invalid key list parameters")
		return
	}
	teamID, digest, alias := query.Get("team_id"), query.Get("key_hash"), strings.ToLower(query.Get("key_alias"))
	orgID := query.Get("organization_id")
	userID := query.Get("user_id")
	projectID, groupID := query.Get("project_id"), query.Get("access_group_id")
	registry.mu.RLock()
	caller, valid := registry.identifyReadCallerLocked(r)
	if !valid {
		registry.mu.RUnlock()
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin && (caller.user == "" || (userID != "" && userID != caller.user) ||
		(teamID != "" && memberIndex(registry.state.Teams[teamID], caller.user) < 0) ||
		(orgID != "" && organizationRole(registry.state.Organizations[orgID], caller.user) == "")) {
		registry.mu.RUnlock()
		modelError(w, http.StatusForbidden, "Key filter is outside the caller's scope")
		return
	}
	keys := make([]keyRecord, 0, len(registry.state.Keys))
	collect := func(token string, key virtualKey, deletedAt *time.Time, deletedBy string) {
		effectiveTeam := key.TeamID
		if effectiveTeam == "" && key.ProjectID != "" {
			effectiveTeam = registry.state.Projects[key.ProjectID].TeamID
		}
		owner := registry.state.Teams[effectiveTeam]
		visible := strings.Contains(strings.ToLower(key.UserID), strings.ToLower(userID))
		if !caller.admin {
			member := memberIndex(owner, caller.user)
			visible = key.UserID == caller.user || (includeCreated && key.CreatedBy == caller.user) ||
				(!owner.Blocked && member >= 0 && ((includeTeams && (owner.Members[member].Role == "admin" || owner.hasMemberPermission("/key/list"))) ||
					((includeTeams || includeCreated) && key.UserID == "")))
		}
		if visible && (orgID == "" || key.OrgID == orgID || owner.OrgID == orgID) &&
			(teamID == "" || effectiveTeam == teamID) && (digest == "" || token == digest) &&
			(projectID == "" || key.ProjectID == projectID) &&
			(groupID == "" || slices.Contains(key.AccessGroupIDs, groupID)) &&
			strings.Contains(strings.ToLower(key.Alias), alias) {
			key.ImportedSpend = nil
			key.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
			record := keyRecord{Token: token, virtualKey: key, DeletedAt: deletedAt, DeletedBy: deletedBy}
			if expandUser {
				if user, exists := registry.state.Users[key.UserID]; exists {
					user.ImportedSpend = nil
					record.User = &user
				}
				if creator, exists := registry.state.Users[key.CreatedBy]; exists {
					record.Creator = &keyRecordCreator{ID: creator.ID, Email: creator.Email, Alias: creator.Alias}
				}
			}
			keys = append(keys, record)
		}
	}
	if status == "deleted" {
		for token, key := range registry.state.DeletedKeys {
			collect(token, key.virtualKey, &key.DeletedAt, key.DeletedBy)
		}
	} else {
		for token, key := range registry.state.Keys {
			collect(token, key, nil, "")
		}
	}
	registry.mu.RUnlock()
	slices.SortFunc(keys, func(a, b keyRecord) int {
		var byColumn int
		switch column {
		case "key_alias":
			byColumn = cmp.Compare(a.Alias, b.Alias)
		case "user_id":
			byColumn = cmp.Compare(a.UserID, b.UserID)
		default:
			var left, right time.Time
			if a.CreatedAt != nil {
				left = *a.CreatedAt
			}
			if b.CreatedAt != nil {
				right = *b.CreatedAt
			}
			byColumn = left.Compare(right)
		}
		if byColumn == 0 {
			byColumn = cmp.Compare(a.Token, b.Token)
		}
		if order != "asc" {
			return -byColumn
		}
		return byColumn
	})
	total := len(keys)
	pages := (total + size - 1) / size
	if page > pages {
		keys = keys[:0]
	} else {
		start := (page - 1) * size
		keys = keys[start:min(start+size, total)]
	}
	var result any = keys
	if !full {
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			ids = append(ids, key.Token)
		}
		result = ids
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"keys": result, "total_count": total, "current_page": page, "total_pages": pages, "size": size,
	})
}

func optionalKeyListBool(query url.Values, name string) (bool, error) {
	if !query.Has(name) {
		return false, nil
	}
	return strconv.ParseBool(query.Get(name))
}
