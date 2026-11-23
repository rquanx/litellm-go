package runtime

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
)

type LegacyFileLookup func(context.Context, string, []string, bool) (map[string]string, error)

func isLegacyManagedFile(value string) bool {
	encoded, ok := strings.CutPrefix(value, "file-")
	if !ok || len(encoded) > 8192 {
		return false
	}
	for _, codec := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		raw, err := codec.DecodeString(strings.TrimRight(encoded, "="))
		if err == nil && strings.HasPrefix(string(raw), "litellm_proxy:") && strings.Contains(string(raw), ";unified_id,") {
			return true
		}
	}
	return false
}

func (s *Server) resolveLegacyFile(r *http.Request, id, alias string, scope []string) (Candidate, string, bool) {
	if s.config.LegacyFiles == nil {
		return Candidate{}, "", false
	}
	var owners []string
	master := s.authorized(r)
	if !master && s.config.ResourceOwnerScopes != nil {
		owners = s.config.ResourceOwnerScopes(ProxyCredential(r))
	}
	if !master && len(owners) == 0 {
		return Candidate{}, "", false
	}
	mappings, err := s.config.LegacyFiles(r.Context(), id, owners, master)
	if err != nil || len(mappings) == 0 {
		return Candidate{}, "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	var original string
	for name, route := range s.configuredRoutes {
		if (alias != "" && alias != name) || !modelAllowed(scope, name) {
			continue
		}
		for _, candidate := range route.Candidates {
			if candidate.DeploymentID == "" || !candidateAllowed(scope, candidate) ||
				(candidate.Provider != "openai" && candidate.Provider != "azure") {
				continue
			}
			value := mappings[candidate.DeploymentID]
			if value == "" && alias != "" && len(route.Candidates) == 1 {
				value = mappings[name]
			}
			if !validResourceSegment(value) {
				continue
			}
			if selected.DeploymentID != "" && selected.DeploymentID != candidate.DeploymentID {
				return Candidate{}, "", false
			}
			selected, original = candidate, value
		}
	}
	return selected, original, selected.DeploymentID != ""
}

func decodeLegacyBatchID(id string) (string, string, bool) {
	encoded, found := strings.CutPrefix(id, "batch_")
	if !found || len(encoded) > 8192 {
		return "", "", false
	}
	for _, codec := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding} {
		raw, err := codec.DecodeString(strings.TrimRight(encoded, "="))
		if err != nil {
			continue
		}
		fields, ok := strings.CutPrefix(string(raw), "litellm_proxy;model_id:")
		if !ok {
			continue
		}
		model, original, ok := strings.Cut(fields, ";llm_batch_id:")
		if ok && model != "" && validResourceSegment(original) {
			return model, original, true
		}
	}
	return "", "", false
}

func (s *Server) legacyBatchDeployment(alias, model string, scope []string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matched string
	for name, route := range s.configuredRoutes {
		if (alias != "" && alias != name) || !modelAllowed(scope, name) {
			continue
		}
		for _, candidate := range route.Candidates {
			if (candidate.Provider != "openai" && candidate.Provider != "azure") || !candidateAllowed(scope, candidate) ||
				(candidate.DeploymentID != model && name != model) || candidate.DeploymentID == "" {
				continue
			}
			if matched != "" && matched != candidate.DeploymentID {
				return "", false
			}
			matched = candidate.DeploymentID
		}
	}
	return matched, matched != ""
}
