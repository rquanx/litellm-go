package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

func ImportLiteLLMAccess(db *sql.DB) (int, error) {
	store := &postgresState{db: db, name: "keys"}
	state := accessState{Keys: map[string]virtualKey{}, Teams: map[string]team{},
		Organizations: map[string]organization{}, Users: map[string]internalUser{}}
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go access state")
	}
	if len(state.Keys)+len(state.Teams)+len(state.Users)+len(state.Organizations) != 0 {
		return 0, errors.New("Go access state must be empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return 0, errors.New("could not open a snapshot of LiteLLM access state")
	}
	defer tx.Rollback()
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
		{"LiteLLM_OrganizationTable", importer.importOrganization},
		{"LiteLLM_UserTable", state.importUser},
		{"LiteLLM_TeamTable", func(payload []byte) error {
			payload, err := importer.expandObjectPermission(payload)
			if err != nil {
				return err
			}
			return state.importTeam(payload)
		}},
		{"LiteLLM_TeamMembership", importer.importTeamMembership},
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
			id, key, blocked, err := decodeLiteLLMKeyForState(payload, &state)
			if err == nil && !blocked {
				state.Keys[id] = key
			}
			return err
		}},
	}
	for _, reader := range readers {
		if err := readAccessRows(ctx, tx, reader.relation, reader.apply); err != nil {
			return 0, err
		}
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
	for _, key := range []string{"policies", "guardrails", "team_member_budget_id", "model_rpm_limit", "model_tpm_limit",
		"team_member_key_duration", "allowed_passthrough_routes", "allowed_vector_store_indexes"} {
		switch string(metadata[key]) {
		case "", "null", "[]", "{}", "\"\"", "false":
		default:
			return fmt.Errorf("unsupported access restriction in metadata.%s", key)
		}
	}
	return nil
}

func (importer accessImporter) importOrganization(payload []byte) error {
	var input struct {
		organization
		BudgetID string `json:"budget_id"`
	}
	if err := decodeAccessOwner(payload, &input); err != nil {
		return err
	}
	models, err := importer.memberBudgetModels(input.BudgetID)
	if err != nil || len(models) != 0 {
		return errors.New("organization uses an unsupported budget")
	}
	record := input.organization
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid organization ID or model scope")
	}
	importer.Organizations[record.ID] = record
	return nil
}

func (state *accessState) importUser(payload []byte) error {
	var record internalUser
	if err := decodeAccessOwner(payload, &record, "organization_id", "team_id", "sso_user_id", "password"); err != nil {
		return err
	}
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid user ID or model scope")
	}
	if record.Role != "internal_user" && record.Role != "internal_user_viewer" {
		return errors.New("unsupported imported user role")
	}
	state.Users[record.ID] = record
	return nil
}

func (state *accessState) importTeam(payload []byte) error {
	var input struct {
		team
		RawMembers json.RawMessage `json:"members_with_roles"`
		Admins []string `json:"admins"`
		Users []string `json:"members"`
	}
	if err := decodeAccessOwner(payload, &input); err != nil {
		return err
	}
	record := input.team
	if record.ID == "" || !validOwnerModels(record.Models) {
		return errors.New("invalid team ID or model scope")
	}
	if _, exists := state.Organizations[record.OrgID]; record.OrgID != "" && !exists {
		return errors.New("team organization is missing")
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

func (importer accessImporter) importTeamMembership(payload []byte) error {
	var input struct {
		teamMembership
		BudgetID string `json:"budget_id"`
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
	models, err := importer.memberBudgetModels(input.BudgetID)
	if err != nil {
		return err
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
