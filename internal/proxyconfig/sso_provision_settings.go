package proxyconfig

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
)

func (registry *KeyRegistry) registerSSOProvisionSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /get/internal_user_settings", registry.internalUserDefaults)
	mux.HandleFunc("PATCH /update/internal_user_settings", registry.internalUserDefaults)
	mux.HandleFunc("GET /get/default_team_settings", registry.defaultTeamSettings)
	mux.HandleFunc("PATCH /update/default_team_settings", registry.defaultTeamSettings)
}

func (registry *KeyRegistry) internalUserDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if !registry.requireAdminRead(w, r) {
			return
		}
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		defaults := registry.ssoUserDefaultsLocked()
		if defaults == nil {
			defaults = &SSOUserDefaults{Role: "internal_user_viewer"}
		}
		modelJSON(w, http.StatusOK, map[string]any{"values": defaults, "field_schema": map[string]any{
			"description": "Default parameters for new internal users",
			"properties": map[string]any{
				"user_role": map[string]any{"type": "string", "description": "Default role assigned to new users created"},
				"models": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
				"teams": map[string]any{"type": "array", "description": "Default teams for new users created"},
				"max_budget": map[string]string{"type": "number", "description": "Default maximum budget (in USD)"},
				"budget_duration": map[string]string{"type": "string", "description": "Default budget duration for new users"},
			},
		}})
		return
	}
	if !registry.requireMaster(w, r) {
		return
	}
	var input *SSOUserDefaults
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 128<<10), &input) != nil || input == nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid internal user defaults")
		return
	}
	if input.Role == "" {
		input.Role = "internal_user_viewer"
	}
	if err := input.validate(); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if input.MaxBudget != nil && registry.keySpend == nil {
		modelError(w, http.StatusBadRequest, "Default user budgets require PostgreSQL spend tracking")
		return
	}
	for _, grant := range input.Teams {
		if entry, found := registry.state.Teams[grant.ID]; !found || entry.Blocked {
			modelError(w, http.StatusBadRequest, "Unknown or blocked default team")
			return
		}
	}
	next := registry.state
	next.SSOUserDefaults = input
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{"status": "success", "settings": input,
			"message": "Internal user settings updated successfully"})
	}
}


type ssoTeamDefaults struct {
	Models *[]string `json:"models" yaml:"models"`
	MaxBudget *float64 `json:"max_budget" yaml:"max_budget"`
	BudgetDuration string `json:"budget_duration" yaml:"budget_duration"`
	RPM *int `json:"rpm_limit" yaml:"rpm_limit"`
	TPM *int `json:"tpm_limit" yaml:"tpm_limit"`
	Permissions *[]string `json:"team_member_permissions" yaml:"team_member_permissions"`
}

func (registry *KeyRegistry) ssoTeamDefaultsLocked() *ssoTeamDefaults {
	if registry.state.SSOTeamDefaults != nil {
		return registry.state.SSOTeamDefaults
	}
	return registry.ssoTeamDefaults
}

func (defaults *ssoTeamDefaults) applyTo(input *teamChanges) {
	if defaults == nil {
		return
	}
	if input.Models == nil {
		input.Models = defaults.Models
	}
	if input.MemberPermissions == nil && defaults.Permissions != nil {
		permissions := []string{"/key/info", "/key/health"}
		for _, route := range *defaults.Permissions {
			if !slices.Contains(permissions, route) {
				permissions = append(permissions, route)
			}
		}
		input.MemberPermissions = &permissions
	}
	if input.MaxBudget == nil && defaults.MaxBudget != nil {
		input.MaxBudget, _ = json.Marshal(defaults.MaxBudget)
	}
	if input.BudgetDuration == nil && defaults.BudgetDuration != "" {
		input.BudgetDuration, _ = json.Marshal(defaults.BudgetDuration)
	}
	if input.RPM == nil && defaults.RPM != nil {
		input.RPM, _ = json.Marshal(defaults.RPM)
	}
	if input.TPM == nil && defaults.TPM != nil {
		input.TPM, _ = json.Marshal(defaults.TPM)
	}
}

func (defaults *ssoTeamDefaults) validate() error {
	if defaults == nil {
		return nil
	}
	if (defaults.Models != nil && !validOwnerModels(*defaults.Models)) ||
		(defaults.Permissions != nil && !validTeamPermissions(*defaults.Permissions)) ||
		(defaults.RPM != nil && *defaults.RPM < 0) || (defaults.TPM != nil && *defaults.TPM < 0) {
		return errors.New("invalid default team parameters")
	}
	user := SSOUserDefaults{MaxBudget: defaults.MaxBudget, BudgetDuration: defaults.BudgetDuration}
	return user.validate()
}

func (registry *KeyRegistry) defaultTeamSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if !registry.requireAdminRead(w, r) {
			return
		}
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		defaults := registry.ssoTeamDefaultsLocked()
		if defaults == nil {
			empty := []string{}
			defaults = &ssoTeamDefaults{Models: &empty}
		}
		modelJSON(w, http.StatusOK, map[string]any{"values": defaults, "field_schema": map[string]any{
			"description": "Default parameters for new teams",
			"properties": map[string]any{
				"models": map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
				"max_budget": map[string]string{"type": "number", "description": "Default maximum budget in USD"},
				"budget_duration": map[string]string{"type": "string"},
				"rpm_limit": map[string]string{"type": "integer"},
				"tpm_limit": map[string]string{"type": "integer"},
				"team_member_permissions": map[string]any{"type": "array", "items": map[string]any{"type":"string","enum":teamMemberPermissions}},
			},
		}})
		return
	}
	if !registry.requireMaster(w, r) {
		return
	}
	var input *ssoTeamDefaults
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 128<<10), &input) != nil || input == nil {
		modelError(w, http.StatusUnprocessableEntity, "Invalid default team settings")
		return
	}
	if err := input.validate(); err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if input.MaxBudget != nil && registry.keySpend == nil {
		modelError(w, http.StatusBadRequest, "Default budgets require PostgreSQL spend tracking")
		return
	}
	next := registry.state
	next.SSOTeamDefaults = input
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{"status": "success", "settings": input, "message": "Default team settings updated successfully"})
	}
}

func (registry *KeyRegistry) SetSSOTeamDefaults(defaults *ssoTeamDefaults) {
	registry.ssoTeamDefaults = defaults
}
