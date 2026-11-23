package runtime

import (
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

func (s *Server) serveVertexPassThrough(w http.ResponseWriter, r *http.Request, suffix string) bool {
	if suffix == "" || !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "PATCH"}, r.Method) {
		return false
	}
	var scope []string
	if !s.authorized(r) {
		var valid bool
		if s.config.AuthorizeKey != nil {
			scope, valid = s.config.AuthorizeKey(ProxyCredential(r))
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
	}
	endpoint, discovery := strings.CutPrefix(suffix, "discovery/")
	if !discovery {
		endpoint = suffix
	}
	segments := strings.Split(endpoint, "/")
	if len(segments) < 5 || segments[1] != "projects" || segments[3] != "locations" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Vertex path requires a project and location")
		return true
	}
	var candidate Candidate
	var resolvedAlias bool
	var originalModel, action string
	modelIndex := -1
	for i := 5; i+1 < len(segments); i++ {
		if segments[i] == "models" {
			modelIndex = i + 1
			originalModel, action, _ = strings.Cut(segments[modelIndex], ":")
			break
		}
	}
	s.mu.RLock()
	if modelIndex >= 0 && modelAllowed(scope, originalModel) {
		if route, found := s.configuredRoutes[originalModel]; found {
			for _, deployment := range route.Candidates {
				if deployment.Vertex != nil && deployment.UseInPassThrough && candidateAllowed(scope, deployment) {
					candidate = deployment
					resolvedAlias = true
					break
				}
			}
		}
	}
	if candidate.Vertex == nil && modelAllowed(scope, "*") {
		for alias, route := range s.configuredRoutes {
			for _, deployment := range route.Candidates {
				if deployment.Vertex == nil || !deployment.UseInPassThrough ||
					(deployment.RoutingGroup != "" && deployment.RoutingGroup != alias) ||
					deployment.Vertex.Project != segments[2] || deployment.Vertex.Location != segments[4] {
					continue
				}
				if candidate.DeploymentID == "" || deployment.DeploymentID < candidate.DeploymentID {
					candidate = deployment
				}
			}
		}
	}
	s.mu.RUnlock()
	if candidate.Vertex == nil {
		writeError(w, http.StatusForbidden, "model_not_allowed", "No authorized Vertex pass-through deployment for this path")
		return true
	}
	if !modelAllowed(scope, "*") && (r.Method != http.MethodPost || !slices.Contains([]string{
		"generateContent", "streamGenerateContent", "countTokens", "predict", "rawPredict", "streamRawPredict",
	}, action)) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "This Vertex operation requires unrestricted model access")
		return true
	}
	segments[2], segments[4] = candidate.Vertex.Project, candidate.Vertex.Location
	if resolvedAlias {
		segments[modelIndex] = candidate.ModelID
		if action != "" {
			segments[modelIndex] += ":" + action
		}
	}
	base := candidate.APIBase
	if discovery {
		base = vertexDiscoveryBase(candidate)
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "https" && target.Scheme != "http") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex provider base")
		return true
	}
	for _, segment := range segments {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Vertex path")
			return true
		}
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.Join(segments, "/")
	target.RawPath = ""
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	route := PassThroughRoute{Path: r.URL.Path, Target: *target, MergeQuery: true,
		authorize: func(upstream *http.Request, _ []byte) error {
			return applyGeminiCredential(upstream, candidate, key)
		}}
	if strings.HasPrefix(action, "stream") {
		route.UpstreamQuery = url.Values{"alt": {"sse"}}
	}
	s.forwardPassThrough(w, r, route)
	return true
}
