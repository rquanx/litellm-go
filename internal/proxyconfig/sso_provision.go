package proxyconfig

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

type SSOUserDefaults struct {
	Role   string           `yaml:"user_role"`
	Models []string         `yaml:"models"`
	Teams  []ssoDefaultTeam `yaml:"teams"`
}

type ssoDefaultTeam struct {
	ID   string `yaml:"team_id"`
	Role string `yaml:"user_role"`
}

func (team *ssoDefaultTeam) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&team.ID)
	}
	type fields ssoDefaultTeam
	return node.Decode((*fields)(team))
}

func (defaults *SSOUserDefaults) enabled() bool {
	return defaults != nil && defaults.Role == "internal_user"
}

func (defaults *SSOUserDefaults) validate() error {
	if defaults == nil {
		return nil
	}
	if !slices.Contains([]string{"", "internal_user", "internal_user_viewer"}, defaults.Role) || !validOwnerModels(defaults.Models) {
		return errors.New("invalid default_internal_user_params role or models")
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

func (registry *KeyRegistry) SetSSOUserDefaults(defaults *SSOUserDefaults) {
	registry.ssoDefaults = defaults
}

// bindSSOUser prepares an atomic account, team membership and subject binding update.
func (s *uiSSOServer) bindSSOUser(w http.ResponseWriter, user internalUser, bound bool, binding, subject string) (accessState, bool) {
	next := s.keys.state
	if bound {
		return next, true
	}
	_, existing := next.Users[user.ID]
	metadata := maps.Clone(user.Metadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	identities, _ := metadata["sso_subjects"].(map[string]any)
	identities = maps.Clone(identities)
	if identities == nil {
		identities = make(map[string]any)
	}
	identities[binding] = subject
	metadata["sso_subjects"] = identities
	user.Metadata = metadata
	next.Users = maps.Clone(next.Users)
	if next.Users == nil {
		next.Users = make(map[string]internalUser)
	}
	next.Users[user.ID] = user
	if existing || !s.keys.ssoDefaults.enabled() || len(s.keys.ssoDefaults.Teams) == 0 {
		return next, true
	}
	next.Teams = maps.Clone(next.Teams)
	for _, grant := range s.keys.ssoDefaults.Teams {
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
	return next, true
}
