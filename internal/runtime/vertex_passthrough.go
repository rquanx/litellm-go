package runtime

import (
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

func (s *Server) serveVertexPassThrough(w http.ResponseWriter, r *http.Request, suffix string) bool {
	if suffix == "live" {
		return s.serveVertexLive(w, r)
	}
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
	var scopedDiscovery bool
	if discovery && s.config.VectorRegistryLookup != nil && len(segments) >= 9 && segments[5] == "collections" && segments[7] == "dataStores" {
		provider, params, found, allowed, err := s.config.VectorRegistryLookup(r, segments[8])
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not read Discovery Engine registration")
			return true
		}
		if found {
			if !allowed || provider != "vertex_ai/search_api" {
				writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Discovery Engine datastore")
				return true
			}
			if !modelAllowed(scope, "*") && (r.Method != http.MethodPost || len(segments) != 11 ||
				segments[9] != "servingConfigs" || !strings.HasSuffix(segments[10], ":search")) {
				writeError(w, http.StatusForbidden, "model_not_allowed", "The key only allows Discovery Engine search")
				return true
			}
			alias, _ := params["model"].(string)
			selected, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
			if !ok || selected.Vertex == nil || !selected.UseInPassThrough {
				writeError(w, http.StatusForbidden, "model_not_allowed", "No authorized Discovery Engine deployment")
				return true
			}
			collection, _ := params["vertex_collection_id"].(string)
			if collection == "" {
				collection = "default_collection"
			}
			if !vertexRAGComponent.MatchString(collection) {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid registered Discovery Engine collection")
				return true
			}
			segments[6] = collection
			candidate, scopedDiscovery = selected, true
		}
	}
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
	if !discovery && modelIndex >= 0 && modelAllowed(scope, originalModel) {
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
	if !scopedDiscovery && !modelAllowed(scope, "*") && (r.Method != http.MethodPost || !slices.Contains([]string{
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

func (s *Server) serveVertexLive(w http.ResponseWriter, r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Vertex AI Live requires a WebSocket upgrade")
		return true
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
	model := r.URL.Query().Get("model")
	candidate, ok := s.selectGoogleResourceCandidate(model, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusNotFound, "model_not_found", "No authorized Vertex AI Live deployment")
		return true
	}
	if model != "" && !modelAllowed(scope, model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return true
	}
	project := r.URL.Query().Get("vertex_project")
	if project == "" {
		project = candidate.Vertex.Project
	}
	location := r.URL.Query().Get("vertex_location")
	if location == "" {
		location = candidate.Vertex.Location
	}
	if project == "" || location == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Vertex project and location are required")
		return true
	}
	base := strings.TrimRight(candidate.APIBase, "/")
	if base == "" {
		base = "https://" + location + "-aiplatform.googleapis.com"
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex provider base")
		return true
	}
	target.Scheme = "wss"
	target.Path = "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	authRequest, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err := applyGeminiCredential(authRequest, candidate, key); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return true
	}
	authRequest.Header.Set("Content-Type", "application/json")
	authRequest.Header.Set("x-goog-user-project", project)
	s.forwardPassThrough(w, r, PassThroughRoute{Path: r.URL.Path, Target: *target, Headers: authRequest.Header})
	return true
}
