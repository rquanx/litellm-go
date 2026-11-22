package runtime

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

func (s *Server) serveBedrockPassThrough(w http.ResponseWriter, r *http.Request, suffix string) bool {
	resource, ok := strings.CutPrefix(suffix, "model/")
	if !ok || r.Method != http.MethodPost {
		return false
	}
	split := strings.LastIndexByte(resource, '/')
	if split < 1 {
		return false
	}
	model, action := resource[:split], resource[split+1:]
	if !slices.Contains([]string{"invoke", "invoke-with-response-stream", "converse", "converse-stream", "count-tokens"}, action) {
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
	if !modelAllowed(scope, model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return true
	}
	s.mu.RLock()
	route, configured := s.configuredRoutes[model]
	s.mu.RUnlock()
	var candidate Candidate
	for _, deployment := range route.Candidates {
		if deployment.Provider == "bedrock" && candidateAllowed(scope, deployment) {
			candidate = deployment
			break
		}
	}
	if candidate.ModelID == "" {
		if configured || !modelAllowed(scope, "*") {
			writeError(w, http.StatusNotFound, "model_not_found", "No Bedrock deployment for this model")
			return true
		}
		cfg, err := awsconfig.LoadDefaultConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "Could not load AWS configuration")
			return true
		}
		region := os.Getenv("AWS_REGION_NAME")
		if region == "" {
			region = cfg.Region
		}
		if region == "" {
			writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock region is missing")
			return true
		}
		base := os.Getenv("AWS_BEDROCK_RUNTIME_ENDPOINT")
		if base == "" {
			base = "https://bedrock-runtime." + region + ".amazonaws.com"
		}
		candidate = Candidate{Provider: "bedrock", APIBase: base, ModelID: model, APIKey: os.Getenv("AWS_BEARER_TOKEN_BEDROCK"),
			Bedrock: &BedrockConfig{Region: region, Credentials: cfg.Credentials}}
	}
	target, err := url.Parse(candidate.APIBase)
	if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock provider base")
		return true
	}
	if strings.Contains(candidate.ModelID, "\\") || path.Clean(candidate.ModelID) != candidate.ModelID ||
		candidate.ModelID == "." || candidate.ModelID == ".." || strings.HasPrefix(candidate.ModelID, "../") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock model")
		return true
	}
	target.RawPath = strings.TrimRight(target.EscapedPath(), "/") + "/model/" + url.PathEscape(candidate.ModelID) + "/" + action
	target.Path = strings.TrimRight(target.Path, "/") + "/model/" + candidate.ModelID + "/" + action
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	s.forwardPassThrough(w, r, PassThroughRoute{Path: r.URL.Path, Target: *target, MergeQuery: true,
		authorize: func(upstream *http.Request, body []byte) error {
			return authorizeBedrock(upstream.Context(), upstream, candidate, body, key)
		}})
	return true
}
