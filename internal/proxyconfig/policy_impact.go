package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
)

type attachmentImpact struct {
	KeyCount     int      `json:"affected_keys_count"`
	TeamCount    int      `json:"affected_teams_count"`
	UnnamedKeys  int      `json:"unnamed_keys_count"`
	UnnamedTeams int      `json:"unnamed_teams_count"`
	KeySamples   []string `json:"sample_keys"`
	TeamSamples  []string `json:"sample_teams"`
}

func estimatePolicyImpact(w http.ResponseWriter, r *http.Request, keys *KeyRegistry) {
	var spec policyAttachment
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := decodeModelJSON(r.Body, &spec); err != nil || spec.Policy == "" || (spec.Scope != "" && spec.Scope != "*") {
		modelError(w, http.StatusUnprocessableEntity, "Expected a policy name and a valid attachment scope")
		return
	}
	if keys == nil {
		modelError(w, http.StatusInternalServerError, "Key registry not connected")
		return
	}
	modelJSON(w, http.StatusOK, keys.policyImpact(spec))
}

func (keys *KeyRegistry) policyImpact(spec policyAttachment) attachmentImpact {
	if spec.Scope == "*" {
		return attachmentImpact{
			KeyCount: -1, TeamCount: -1,
			KeySamples:  []string{"(global scope — affects all keys)"},
			TeamSamples: []string{"(global scope — affects all teams)"},
		}
	}
	result := attachmentImpact{}
	teamsByAlias := make(map[string]bool)
	matchingTeams := make(map[string]bool)
	keysByAlias := make(map[string]bool)
	keys.mu.RLock()
	for id, team := range keys.state.Teams {
		aliasMatch := len(spec.Teams) > 0 && team.Alias != "" && policyMatches(spec.Teams, team.Alias)
		if aliasMatch {
			matchingTeams[id] = true
		}
		if aliasMatch || policyMetadataMatches(spec.Tags, team.Metadata) {
			if team.Alias == "" {
				result.UnnamedTeams++
			} else {
				teamsByAlias[team.Alias] = true
			}
		}
	}
	for _, key := range keys.state.Keys {
		teamID := key.TeamID
		if teamID == "" && key.ProjectID != "" {
			teamID = keys.state.Projects[key.ProjectID].TeamID
		}
		if matchingTeams[teamID] || (len(spec.Keys) > 0 && key.Alias != "" && policyMatches(spec.Keys, key.Alias)) ||
			policyMetadataMatches(spec.Tags, key.Metadata) {
			if key.Alias == "" {
				result.UnnamedKeys++
			} else {
				keysByAlias[key.Alias] = true
			}
		}
	}
	keys.mu.RUnlock()
	result.KeyCount = len(keysByAlias) + result.UnnamedKeys
	result.TeamCount = len(teamsByAlias) + result.UnnamedTeams
	result.KeySamples = policyImpactSamples(keysByAlias)
	result.TeamSamples = policyImpactSamples(teamsByAlias)
	return result
}

func policyMetadataMatches(patterns []string, metadata map[string]any) bool {
	return len(patterns) > 0 && slices.ContainsFunc(policyMetadataTags(metadata), func(tag string) bool {
		return policyMatches(patterns, tag)
	})
}

func policyImpactSamples(names map[string]bool) []string {
	samples := slices.Sorted(maps.Keys(names))
	return append([]string{}, samples[:min(len(samples), 10)]...)
}
