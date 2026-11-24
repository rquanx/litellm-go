package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/redis/go-redis/v9"
)

func ImportLiteLLMAccess(db *sql.DB) (int, error) {
	return ImportLiteLLMAccessWithRedis(db, nil, "")
}

func ImportLiteLLMAccessWithRedis(db *sql.DB, counters *redis.Client, namespace string) (int, error) {
	store := &postgresState{db: db, name: "keys"}
	state := accessState{Keys: map[string]virtualKey{}, Teams: map[string]team{},
		Organizations: map[string]organization{}, Users: map[string]internalUser{}}
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go access state")
	}
	if len(state.Keys)+len(state.Teams)+len(state.Users)+len(state.Organizations)+len(state.Projects)+
		len(state.AccessGroups)+len(state.DeletedKeys) != 0 {
		return 0, errors.New("Go access state must be empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	snapshot := time.Now().UTC()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, errors.New("could not open a snapshot of LiteLLM access state")
	}
	defer tx.Rollback()
	groupViews := make(map[string]accessGroupView)
	importer := accessImporter{accessState: &state, budgets: make(map[string][]byte), objectPermissions: make(map[string][]byte)}
	var hasPermissions bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('"LiteLLM_ObjectPermissionTable"') IS NOT NULL`).Scan(&hasPermissions); err != nil {
		return 0, errors.New("could not locate LiteLLM object permissions")
	}
	if hasPermissions {
		if err := readAccessRows(ctx, tx, "LiteLLM_ObjectPermissionTable", importer.importObjectPermission); err != nil {
			return 0, err
		}
	}
	readers := []struct {
		relation string
		apply    func([]byte) error
	}{
		{"LiteLLM_BudgetTable", importer.importBudget},
		{"LiteLLM_OrganizationTable", func(payload []byte) error { return importer.importOrganization(payload, snapshot) }},
		{"LiteLLM_UserTable", func(payload []byte) error { return state.importUser(payload, snapshot) }},
		{"LiteLLM_InvitationLink", state.importDashboardInvitation},
		{"LiteLLM_AccessGroupTable", func(payload []byte) error {
			group, err := decodeImportedAccessGroup(payload)
			if err != nil {
				return err
			}
			if state.AccessGroups == nil {
				state.AccessGroups = make(map[string]accessGroup)
			}
			state.AccessGroups[group.ID] = group.accessGroup
			if len(group.Teams) > 0 {
				if state.AccessGroupTeamOverrides == nil {
					state.AccessGroupTeamOverrides = make(map[string][]string)
				}
				state.AccessGroupTeamOverrides[group.ID] = group.Teams
			}
			groupViews[group.ID] = group
			return nil
		}},
		{"LiteLLM_TeamTable", func(payload []byte) error {
			payload, err := importer.expandObjectPermission(payload)
			if err != nil {
				return err
			}
			return importer.importTeam(payload, snapshot)
		}},
		{"LiteLLM_TeamMembership", func(payload []byte) error { return importer.importTeamMembership(payload, snapshot) }},
		{"LiteLLM_ProjectTable", func(payload []byte) error { return importer.importProject(payload, snapshot) }},
		{"LiteLLM_OrganizationMembership", importer.importOrganizationMembership},
		{"LiteLLM_VerificationToken", func(payload []byte) error {
			var status struct {
				Blocked bool `json:"blocked"`
			}
			if err := json.Unmarshal(payload, &status); err != nil {
				return err
			}
			if status.Blocked {
				return nil
			}
			payload, err := importer.expandObjectPermission(payload)
			if err != nil {
				return err
			}
			id, key, blocked, err := decodeLiteLLMKeyForState(payload, &state, snapshot)
			if err == nil && !blocked {
				if key.BudgetID != "" {
					if err := validateImportedLinkedBudget(importer.budgets[key.BudgetID], snapshot, true); err != nil {
						return fmt.Errorf("virtual key %q: %w", key.Alias, err)
					}
				}
				state.Keys[id] = key
			}
			return err
		}},
	}
	for _, reader := range readers {
		if reader.relation == "LiteLLM_ProjectTable" || reader.relation == "LiteLLM_InvitationLink" ||
			reader.relation == "LiteLLM_AccessGroupTable" {
			var present bool
			if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, `"`+reader.relation+`"`).Scan(&present); err != nil {
				return 0, err
			}
			if !present {
				continue
			}
		}
		if err := readAccessRows(ctx, tx, reader.relation, reader.apply); err != nil {
			return 0, err
		}
	}
	for digest, key := range state.Keys {
		for _, id := range key.AccessGroupIDs {
			source := groupViews[id]
			if !slices.Contains(source.Keys, digest) {
				key.RestrictedGroupIDs = append(key.RestrictedGroupIDs, id)
			}
		}
		state.Keys[digest] = key
	}
	if err := importBudgetWindowBalances(ctx, &state, counters, namespace, snapshot); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, errors.New("could not finish reading LiteLLM access state")
	}
	if err := store.save(state); err != nil {
		return 0, errors.New("could not commit imported LiteLLM access state")
	}
	return len(state.Keys), nil
}

type accessImporter struct {
	*accessState
	budgets map[string][]byte
	objectPermissions map[string][]byte
}

func (importer accessImporter) importObjectPermission(payload []byte) error {
	var record struct {
		ID string `json:"object_permission_id"`
	}
	if json.Unmarshal(payload, &record) != nil || record.ID == "" {
		return errors.New("invalid object permission ID")
	}
	importer.objectPermissions[record.ID] = slices.Clone(payload)
	return nil
}

func (importer accessImporter) expandObjectPermission(payload []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, errors.New("invalid access record")
	}
	raw := fields["object_permission_id"]
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
		return payload, nil
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || importer.objectPermissions[id] == nil {
		return nil, errors.New("missing or invalid object permission")
	}
	var restrictions map[string]json.RawMessage
	if err := json.Unmarshal(importer.objectPermissions[id], &restrictions); err != nil {
		return nil, errors.New("invalid object permission")
	}
	permission := objectPermission{}
	for name, value := range restrictions {
		switch name {
		case "object_permission_id":
		case "search_tools":
			if err := json.Unmarshal(value, &permission.SearchTools); err != nil {
				return nil, errors.New("invalid search tool scope")
			}
		case "mcp_servers":
			if err := json.Unmarshal(value, &permission.MCPServers); err != nil {
				return nil, errors.New("invalid MCP server scope")
			}
		case "mcp_tool_permissions":
			if err := json.Unmarshal(value, &permission.MCPTools); err != nil {
				return nil, errors.New("invalid MCP tool scope")
			}
		case "mcp_access_groups":
			if err := json.Unmarshal(value, &permission.MCPAccessGroups); err != nil {
				return nil, errors.New("invalid MCP group scope")
			}
		case "agents":
			if err := json.Unmarshal(value, &permission.Agents); err != nil {
				return nil, errors.New("invalid agent scope")
			}
		case "agent_access_groups":
			if err := json.Unmarshal(value, &permission.AgentGroups); err != nil {
				return nil, errors.New("invalid agent group scope")
			}
		case "mcp_toolsets":
			if err := json.Unmarshal(value, &permission.MCPToolsets); err != nil {
				return nil, errors.New("invalid MCP toolset scope")
			}
		default:
			switch string(value) {
			case "null", "[]", "{}", `""`:
			default:
				return nil, fmt.Errorf("unsupported object permission field %s", name)
			}
		}
	}
	delete(fields, "object_permission_id")
	fields["object_permission"], _ = json.Marshal(permission)
	return json.Marshal(fields)
}

func (importer accessImporter) importBudget(payload []byte) error {
	var budget struct {
		ID string `json:"budget_id"`
	}
	if json.Unmarshal(payload, &budget) != nil || budget.ID == "" {
		return errors.New("invalid budget ID")
	}
	importer.budgets[budget.ID] = slices.Clone(payload)
	return nil
}

func (importer accessImporter) memberBudgetModels(id string) ([]string, error) {
	if id == "" {
		return nil, nil
	}
	payload, exists := importer.budgets[id]
	if !exists {
		return nil, errors.New("referenced budget is missing")
	}
	var budget struct {
		Models []string `json:"allowed_models"`
	}
	if err := decodeAccessOwner(payload, &budget); err != nil {
		return nil, fmt.Errorf("referenced budget: %w", err)
	}
	if !validOwnerModels(budget.Models) {
		return nil, errors.New("unsupported budget model scope")
	}
	return budget.Models, nil
}

func readAccessRows(ctx context.Context, tx *sql.Tx, table string, apply func([]byte) error) error {
	// table is selected from the relation names in ImportLiteLLMAccess.
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(r) FROM "`+table+`" AS r`)
	if err != nil {
		return fmt.Errorf("could not read %s", table)
	}
	defer rows.Close()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return fmt.Errorf("could not decode %s row", table)
		}
		if err := apply(payload); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	return rows.Err()
}

var importedOwnerRestrictions = []string{
	"object_permission_id", "max_budget", "soft_budget", "model_max_budget", "max_parallel_requests", "tpm_limit",
	"rpm_limit", "budget_duration", "budget_limits", "access_group_ids", "policies", "allowed_cache_controls",
	"router_settings", "guardrails", "model_id", "team_member_permissions", "default_team_member_models",
}

func decodeAccessOwner(payload []byte, target any, extra ...string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil || json.Unmarshal(payload, target) != nil {
		return errors.New("invalid access record")
	}
	for _, list := range [][]string{importedOwnerRestrictions, extra} {
		for _, key := range list {
			switch string(fields[key]) {
			case "", "null", "[]", "{}", "\"\"", "false":
			default:
				return fmt.Errorf("unsupported access restriction %s", key)
			}
		}
	}
	var metadata map[string]json.RawMessage
	if raw := fields["metadata"]; len(raw) > 0 && json.Unmarshal(raw, &metadata) != nil {
		return errors.New("invalid access metadata")
	}
	for _, key := range []string{"policies", "guardrails", "team_member_budget_id",
		"team_member_key_duration", "allowed_passthrough_routes", "allowed_vector_store_indexes"} {
		switch string(metadata[key]) {
		case "", "null", "[]", "{}", "\"\"", "false":
		default:
			return fmt.Errorf("unsupported access restriction in metadata.%s", key)
		}
	}
	return nil
}

func (importer accessImporter) importOrganization(payload []byte, snapshot time.Time) error {
	var input struct {
		organization
		Spend json.RawMessage `json:"spend"`
	}
	if err := decodeAccessOwner(payload, &input); err != nil {
		return err
	}
	record := input.organization
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid organization ID or model scope")
	}
	balance, err := importer.importLinkedBudget(input.BudgetID, input.Spend, snapshot)
	if err != nil {
		return fmt.Errorf("organization budget: %w", err)
	}
	record.ImportedSpend = balance
	importer.Organizations[record.ID] = record
	return nil
}

func decodeBudgetedOwner(payload []byte, target any, snapshot time.Time, max **float64, period *string, reset **time.Time,
	spend **importedKeySpend, rates *ownerRates, extra ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return errors.New("invalid access record")
	}
	var budget struct {
		Max *float64 `json:"max_budget"`
		Duration string `json:"budget_duration"`
	}
	if err := json.Unmarshal(payload, &budget); err != nil {
		return errors.New("invalid owner budget")
	}
	anchor, balance, err := readImportedBudget(budget.Max, budget.Duration, fields["budget_reset_at"], fields["spend"], snapshot)
	if err != nil {
		return err
	}
	delete(fields, "max_budget")
	delete(fields, "budget_duration")
	delete(fields, "budget_reset_at")
	delete(fields, "imported_spend")
	if rates != nil {
		changes := ownerRateChanges{RPM: fields["rpm_limit"], TPM: fields["tpm_limit"], Parallel: fields["max_parallel_requests"]}
		if err := changes.apply(rates); err != nil {
			return err
		}
		delete(fields, "rpm_limit")
		delete(fields, "tpm_limit")
		delete(fields, "max_parallel_requests")
	}
	payload, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := decodeAccessOwner(payload, target, extra...); err != nil {
		return err
	}
	*max, *period, *reset, *spend = budget.Max, budget.Duration, anchor, balance
	return nil
}

func (state *accessState) importUser(payload []byte, snapshot time.Time) error {
	var credentials struct{ Password string }
	if json.Unmarshal(payload, &credentials) != nil ||
		(credentials.Password != "" && !validDashboardPasswordHash(credentials.Password)) {
		return errors.New("invalid user password hash")
	}
	var record internalUser
	if err := decodeBudgetedOwner(payload, &record, snapshot, &record.MaxBudget, &record.BudgetDuration,
		&record.BudgetResetAt, &record.ImportedSpend, &record.ownerRates, "organization_id", "team_id", "sso_user_id"); err != nil {
		return err
	}
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid user ID or model scope")
	}
	if !dashboardUserRole(record.Role) {
		return errors.New("unsupported imported user role")
	}
	state.Users[record.ID] = record
	if credentials.Password != "" {
		if state.Passwords == nil {
			state.Passwords = make(map[string]string)
		}
		state.Passwords[record.ID] = credentials.Password
	}
	return nil
}

func (state *accessState) importDashboardInvitation(payload []byte) error {
	var fields struct {
		ID string `json:"id"`
		UserID string `json:"user_id"`
		Accepted bool `json:"is_accepted"`
		AcceptedAt json.RawMessage `json:"accepted_at"`
		ExpiresAt json.RawMessage `json:"expires_at"`
		CreatedAt json.RawMessage `json:"created_at"`
		CreatedBy string `json:"created_by"`
		UpdatedAt json.RawMessage `json:"updated_at"`
		UpdatedBy string `json:"updated_by"`
	}
	if err := json.Unmarshal(payload, &fields); err != nil || fields.ID == "" || state.Users[fields.UserID].ID == "" {
		return errors.New("invalid invitation or invitation owner")
	}
	invite := dashboardInvitation{ID: fields.ID, UserID: fields.UserID, Accepted: fields.Accepted,
		CreatedBy: fields.CreatedBy, UpdatedBy: fields.UpdatedBy}
	for _, field := range []struct {
		name string
		value json.RawMessage
		destination *time.Time
	}{
		{"expires_at", fields.ExpiresAt, &invite.ExpiresAt},
		{"created_at", fields.CreatedAt, &invite.CreatedAt},
		{"updated_at", fields.UpdatedAt, &invite.UpdatedAt},
	} {
		value, err := liteLLMTimestamp(field.value)
		if err != nil || value == nil {
			return fmt.Errorf("invalid invitation %s", field.name)
		}
		*field.destination = *value
	}
	var err error
	if invite.AcceptedAt, err = liteLLMTimestamp(fields.AcceptedAt); err != nil {
		return err
	}
	if state.Invitations == nil {
		state.Invitations = make(map[string]dashboardInvitation)
	}
	state.Invitations[invite.ID] = invite
	return nil
}

func (importer accessImporter) importTeam(payload []byte, snapshot time.Time) error {
	state := importer.accessState
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return errors.New("invalid team record")
	}
	settings, err := importRouterSettings(fields)
	if err != nil {
		return err
	}
	var groups []string
	if raw := fields["access_group_ids"]; len(raw) > 0 &&
		(json.Unmarshal(raw, &groups) != nil || !state.validAccessGroupIDs(groups)) {
		return errors.New("invalid team access group reference")
	}
	delete(fields, "access_group_ids")
	var memberPermissions *[]string
	if raw := fields["team_member_permissions"]; len(raw) > 0 &&
		(json.Unmarshal(raw, &memberPermissions) != nil || (memberPermissions != nil && !validTeamPermissions(*memberPermissions))) {
		return errors.New("invalid imported team member permissions")
	}
	delete(fields, "team_member_permissions")
	windows, err := decodeImportedBudgetWindows(fields["budget_limits"], snapshot)
	if err != nil {
		return err
	}
	delete(fields, "budget_limits")
	var metadata map[string]json.RawMessage
	if raw := fields["metadata"]; len(raw) != 0 && json.Unmarshal(raw, &metadata) != nil {
		return errors.New("invalid team metadata")
	}
	var defaultBudgetID string
	if raw := metadata["team_member_budget_id"]; len(raw) != 0 {
		if json.Unmarshal(raw, &defaultBudgetID) != nil {
			return errors.New("invalid default team member budget ID")
		}
		delete(metadata, "team_member_budget_id")
		fields["metadata"], _ = json.Marshal(metadata)
	}
	payload, _ = json.Marshal(fields)
	var input struct {
		team
		RawMembers json.RawMessage `json:"members_with_roles"`
		Admins []string `json:"admins"`
		Users []string `json:"members"`
	}
	if err := decodeBudgetedOwner(payload, &input, snapshot, &input.MaxBudget, &input.BudgetDuration,
		&input.BudgetResetAt, &input.ImportedSpend, &input.ownerRates); err != nil {
		return err
	}
	record := input.team
	record.AccessGroupIDs = groups
	record.MemberPermissions = memberPermissions
	record.RouterSettings = settings
	record.BudgetLimits = windows
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid team ID or model scope")
	}
	if _, exists := state.Organizations[record.OrgID]; record.OrgID != "" && !exists {
		return errors.New("team organization is missing")
	}
	if defaultBudgetID != "" {
		budget, exists := importer.budgets[defaultBudgetID]
		if !exists {
			return errors.New("default team member budget is missing")
		}
		var details struct {
			Models []string `json:"allowed_models"`
		}
		var ignored *importedKeySpend
		if err := decodeBudgetedOwner(budget, &details, snapshot, &record.MemberBudget, &record.MemberBudgetDuration,
			&record.MemberBudgetResetAt, &ignored, nil); err != nil || len(details.Models) != 0 {
			return errors.New("unsupported default team member budget")
		}
	}
	switch string(input.RawMembers) {
	case "", "{}", "null":
	default:
		if err := json.Unmarshal(input.RawMembers, &record.Members); err != nil {
			return errors.New("invalid team members")
		}
	}
	for index := range record.Members {
		member := &record.Members[index]
		user, exists := membershipUser(state.Users, member.UserID, member.Email, false)
		if !exists || !slices.Contains([]string{"admin", "user"}, member.Role) || !validOwnerModels(member.Models) {
			return errors.New("unsupported team membership")
		}
		member.UserID, member.TeamID = user.ID, record.ID
	}
	for _, members := range []struct {
		ids []string
		role string
	}{{input.Admins, "admin"}, {input.Users, "user"}} {
		for _, id := range members.ids {
			if _, exists := state.Users[id]; !exists {
				return errors.New("team member user is missing")
			}
			if memberIndex(record, id) < 0 {
				record.Members = append(record.Members, teamMembership{TeamID: record.ID, UserID: id, Role: members.role})
			}
		}
	}
	state.Teams[record.ID] = record
	return nil
}

func (importer accessImporter) importTeamMembership(payload []byte, snapshot time.Time) error {
	var input struct {
		teamMembership
		BudgetID string `json:"budget_id"`
		Spend json.RawMessage `json:"spend"`
	}
	if err := decodeAccessOwner(payload, &input); err != nil {
		return err
	}
	record := input.teamMembership
	owner, exists := importer.Teams[record.TeamID]
	index := memberIndex(owner, record.UserID)
	if !exists || index < 0 {
		return errors.New("missing team membership role or user")
	}
	member := &owner.Members[index]
	var models []string
	if input.BudgetID != "" {
		budgetPayload, exists := importer.budgets[input.BudgetID]
		if !exists {
			return errors.New("referenced budget is missing")
		}
		var budget struct {
			Models []string `json:"allowed_models"`
		}
		var unusedBalance *importedKeySpend
		if err := decodeBudgetedOwner(budgetPayload, &budget, snapshot, &member.MaxBudget, &member.BudgetDuration,
			&member.BudgetResetAt, &unusedBalance, nil); err != nil {
			return fmt.Errorf("referenced membership budget: %w", err)
		}
		if !validOwnerModels(budget.Models) {
			return errors.New("unsupported budget model scope")
		}
		models = budget.Models
	}
	var spend *float64
	if len(input.Spend) != 0 && (json.Unmarshal(input.Spend, &spend) != nil || (spend != nil && *spend < 0)) {
		return errors.New("invalid membership spend")
	}
	if spend != nil {
		budget := owner.memberBudget(record.UserID)
		member.ImportedSpend = &importedKeySpend{Amount: *spend, AsOf: snapshot, ResetAt: budget.BudgetResetAt}
	}
	if models != nil {
		if len(owner.Members[index].Models) != 0 || len(record.Models) != 0 {
			return errors.New("multiple model scopes for one team membership")
		}
		owner.Members[index].Models = models
	} else if record.Models != nil {
		if !validOwnerModels(record.Models) || len(owner.Members[index].Models) != 0 {
			return errors.New("unsupported member model scope")
		}
		owner.Members[index].Models = record.Models
	}
	return nil
}

func (importer accessImporter) importOrganizationMembership(payload []byte) error {
	var input struct {
		organizationMember
		BudgetID string `json:"budget_id"`
	}
	if err := decodeAccessOwner(payload, &input); err != nil {
		return err
	}
	models, err := importer.memberBudgetModels(input.BudgetID)
	if err != nil || len(models) != 0 {
		return errors.New("organization membership uses an unsupported budget")
	}
	record := input.organizationMember
	org, exists := importer.Organizations[record.OrgID]
	if !exists {
		return errors.New("membership organization is missing")
	}
	user, exists := importer.Users[record.UserID]
	if !exists || !slices.Contains([]string{"org_admin", "internal_user", "internal_user_viewer"}, record.Role) {
		return errors.New("unsupported organization membership")
	}
	record.Email = user.Email
	org.Members = append(org.Members, record)
	importer.Organizations[org.ID] = org
	return nil
}

func (state *accessState) containsKeyOwners(key virtualKey) bool {
	if key.ProjectID != "" {
		record, exists := state.Projects[key.ProjectID]
		if !exists || (record.TeamID != "" && key.TeamID != "" && record.TeamID != key.TeamID) {
			return false
		}
		if key.TeamID == "" {
			key.TeamID = record.TeamID
		}
	}
	if _, exists := state.Users[key.UserID]; key.UserID != "" && !exists {
		return false
	}
	if _, exists := state.Organizations[key.OrgID]; key.OrgID != "" && !exists {
		return false
	}
	if key.TeamID != "" {
		owner, exists := state.Teams[key.TeamID]
		if !exists || (key.OrgID != "" && key.OrgID != owner.OrgID) || (key.UserID != "" && memberIndex(owner, key.UserID) < 0) {
			return false
		}
	}
	return true
}

func (importer accessImporter) importProject(payload []byte, snapshot time.Time) error {
	payload, err := importer.expandObjectPermission(payload)
	if err != nil {
		return err
	}
	var record struct {
		project
		Spend json.RawMessage `json:"spend"`
	}
	if err := decodeAccessOwner(payload, &record, "model_rpm_limit", "model_tpm_limit"); err != nil {
		return err
	}
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid project identity or unsupported project policy")
	}
	record.ImportedSpend, err = importer.importLinkedBudget(record.BudgetID, record.Spend, snapshot)
	if err != nil {
		return fmt.Errorf("project budget: %w", err)
	}
	if _, exists := importer.Teams[record.TeamID]; !exists && record.TeamID != "" {
		return errors.New("project team does not exist")
	}
	if importer.Projects == nil {
		importer.Projects = make(map[string]project)
	}
	importer.Projects[record.ID] = record.project
	return nil
}

func (importer accessImporter) importLinkedBudget(id string, rawSpend json.RawMessage, snapshot time.Time) (*importedKeySpend, error) {
	if id == "" {
		return nil, nil
	}
	if err := validateImportedLinkedBudget(importer.budgets[id], snapshot, false); err != nil {
		return nil, err
	}
	_, balance, err := readImportedBudget(nil, "", nil, rawSpend, snapshot)
	return balance, err
}

func validateImportedLinkedBudget(payload []byte, snapshot time.Time, allowKeyRateLimits bool) error {
	if payload == nil {
		return errors.New("referenced budget is missing")
	}
	if allowKeyRateLimits {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
			return errors.New("invalid linked budget")
		}
		for _, name := range []string{"rpm_limit", "tpm_limit", "max_parallel_requests"} {
			raw := fields[name]
			if len(raw) == 0 {
				continue
			}
			var limit *int
			if json.Unmarshal(raw, &limit) != nil || (limit != nil && *limit < 0) {
				return fmt.Errorf("invalid linked budget %s", name)
			}
			delete(fields, name)
		}
		payload, _ = json.Marshal(fields)
	}
	var budget struct {
		Models []string `json:"allowed_models"`
	}
	var max *float64
	var duration string
	var reset *time.Time
	var ignored *importedKeySpend
	if err := decodeBudgetedOwner(payload, &budget, snapshot, &max, &duration, &reset, &ignored, nil); err != nil {
		return err
	}
	if len(budget.Models) != 0 {
		return errors.New("unsupported model scope in linked budget")
	}
	return nil
}
