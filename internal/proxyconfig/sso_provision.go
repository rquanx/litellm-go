package proxyconfig

import (
	"encoding/json"
	"errors"
	"math"
	"time"
	"maps"
	"net/http"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type SSOUserDefaults struct {
	MaxBudget *float64 `json:"max_budget" yaml:"max_budget"`
	BudgetDuration string `json:"budget_duration" yaml:"budget_duration"`
	Role   string           `json:"user_role" yaml:"user_role"`
	Models []string         `json:"models" yaml:"models"`
	Teams  []ssoDefaultTeam `json:"teams" yaml:"teams"`
	AvailableTeams []string `json:"available_teams" yaml:"available_teams"`
}

type ssoDefaultTeam struct {
	ID   string `json:"team_id" yaml:"team_id"`
	Role string `json:"user_role" yaml:"user_role"`
}

func (team *ssoDefaultTeam) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&team.ID)
	}
	type fields ssoDefaultTeam
	return node.Decode((*fields)(team))
}

func (team *ssoDefaultTeam) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &team.ID)
	}
	type fields ssoDefaultTeam
	return decodeModelJSON(strings.NewReader(string(data)), (*fields)(team))
}

func (registry *KeyRegistry) ssoUserDefaultsLocked() *SSOUserDefaults {
	if registry.state.SSOUserDefaults != nil {
		return registry.state.SSOUserDefaults
	}
	return registry.ssoDefaults
}

func (defaults *SSOUserDefaults) enabled() bool {
	return defaults != nil && internalSSORole(defaults.Role)
}

func (defaults *SSOUserDefaults) validate() error {
	if defaults == nil {
		return nil
	}
	if !slices.Contains([]string{"", "internal_user", "internal_user_viewer"}, defaults.Role) || !validOwnerModels(defaults.Models) {
		return errors.New("invalid default_internal_user_params role or models")
	}
	if defaults.MaxBudget != nil && (*defaults.MaxBudget < 0 || math.IsInf(*defaults.MaxBudget, 0) || math.IsNaN(*defaults.MaxBudget)) {
		return errors.New("invalid default internal user budget")
	}
	if defaults.BudgetDuration != "" {
		if _, err := nextBudgetReset(time.Now(), defaults.BudgetDuration); err != nil {
			return errors.New("invalid default internal user budget duration")
		}
	}
	if slices.ContainsFunc(defaults.AvailableTeams, func(id string) bool { return strings.TrimSpace(id) == "" }) {
		return errors.New("invalid available team in default_internal_user_params")
	}
	seen := make(map[string]bool)
	for _, team := range defaults.Teams {
		if strings.TrimSpace(team.ID) == "" || seen[team.ID] || !slices.Contains([]string{"", "admin", "user"}, team.Role) {
			return errors.New("invalid team in default_internal_user_params")
		}
		seen[team.ID] = true
	}
	return nil
}

func (registry *KeyRegistry) SetSSOUserDefaults(defaults *SSOUserDefaults, teamClaim string) {
	registry.ssoDefaults = defaults
	registry.ssoTeamClaim = teamClaim
}

// bindSSOUser prepares an atomic account, team membership and subject binding update.
func (s *uiSSOServer) bindSSOUser(w http.ResponseWriter, user internalUser, bound bool, binding string, identity ssoIdentity) (accessState, bool) {
	next := s.keys.state
	_, existing := next.Users[user.ID]
	if !bound {
		metadata := maps.Clone(user.Metadata)
		if metadata == nil {
			metadata = make(map[string]any)
		}
		identities, _ := metadata["sso_subjects"].(map[string]any)
		identities = maps.Clone(identities)
		if identities == nil {
			identities = make(map[string]any)
		}
		identities[binding] = identity.Subject
		metadata["sso_subjects"] = identities
		user.Metadata = metadata
		next.Users = maps.Clone(next.Users)
		if next.Users == nil {
			next.Users = make(map[string]internalUser)
		}
		next.Users[user.ID] = user
	} else if original := next.Users[user.ID]; original.Role != user.Role {
		next.Users = maps.Clone(next.Users)
		next.Users[user.ID] = user
	}
	var grants []ssoDefaultTeam
	if defaults := s.keys.ssoUserDefaultsLocked(); !existing && defaults.enabled() {
		grants = defaults.Teams
	}
	if len(grants) == 0 && len(identity.TeamIDs) == 0 {
		return next, true
	}
	next.Teams = maps.Clone(next.Teams)
	for _, grant := range grants {
		owner, found := next.Teams[grant.ID]
		if !found || owner.Blocked {
			modelError(w, http.StatusServiceUnavailable, "SSO default team is unavailable")
			return accessState{}, false
		}
		role := grant.Role
		if role == "" {
			role = "user"
		}
		owner.Members = append(slices.Clone(owner.Members), teamMembership{TeamID: owner.ID, UserID: user.ID, Email: user.Email, Role: role})
		next.Teams[grant.ID] = owner
	}
	for _, id := range identity.TeamIDs {
		owner, found := next.Teams[id]
		if !found || owner.Blocked || memberIndex(owner, user.ID) >= 0 {
			continue
		}
		owner.Members = append(slices.Clone(owner.Members), teamMembership{TeamID: id, UserID: user.ID, Email: user.Email, Role: "user"})
		next.Teams[id] = owner
	}
	return next, true
}
