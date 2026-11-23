package runtime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Routes             []Route
	AuthorizeInference bool
	Token              string
	HTTPClient         *http.Client
	MaxBodyBytes       int64
	MaxConcurrent      int
	NumRetries         int
	MaxFallbacks       *int
	AuthorizeKey       func(string) ([]string, bool)
	CustomerPolicy     func(context.Context, string) (CustomerPolicy, error)
	AuthorizeSearch    func(string) ([]string, bool)
	AuthorizeAgent func(string) func(Agent) bool
	AuthorizeMCP       func(string) (MCPAccess, bool)
	AuthorizeMCPToolset func(string) ([]string, MCPAccess, bool)
	ResourceOwnerScopes func(string) []string
	MCPUserOAuthStore MCPUserOAuthStore
	MCPUserOAuthIdentity func(*http.Request) string
	ContainerOwners     ContainerOwners
	LegacyFiles         LegacyFileLookup
	LegacyVectorStores  LegacyFileLookup
	VectorRegistryLookup VectorRegistryLookup
	ResponsePolling     ResponsePollingConfig
	PollingStore        PollingStore
	InferenceCache      InferenceCacheConfig
	InferenceCacheStore InferenceCacheStore
	SearchTools        []SearchTool
	SearchFallbacks    map[string][]string
	RoutingStrategy    string
	RoutingState       RoutingState
	EnableTagFiltering bool
	TagsMatchAll       bool
	LatencyBuffer      float64
	LatencySamples     int
	Callback           func(InferenceEvent)
	LogMessages        bool
	Cooldown           time.Duration
	AllowedFails       int
	RetryPolicy        RetryPolicy
	ModelRetryPolicies map[string]RetryPolicy
	DatabasePing       func(context.Context) error
	HealthHistory      HealthHistory
	SharedHealthChecks bool
	HealthCoordinator HealthCoordinator
	BackgroundHealthInterval time.Duration
	HealthCheckConcurrency int
	SkipDisabledHealthChecks bool
	HealthRoutingStaleness time.Duration
	IgnoreTransientHealthErrors bool
	PassThrough []PassThroughRoute
	MCPServers []MCPServer
	Agents []Agent
	CallbackNames      []string
	PublicReadinessDetails bool
}

type Server struct {
	config Config
	mu     sync.RWMutex
	inFlight atomic.Int64
	routes map[string]Route
	configuredRoutes map[string]Route
	agents []Agent
	searchTools []SearchTool
	mcpServers []MCPServer
	mcpToolsets []MCPToolset
	mcpSSESessions map[string]*clientMCPSession
	mcpStreamableSessions map[string]*mcpStreamableSession
	passThrough []PassThroughRoute
	searchFallbacks map[string][]string
	healthResults map[string]ModelHealthRecord
	configGeneration uint64
	sem    chan struct{}
	veoReads chan struct{}
	trafficMu sync.Mutex
	deploymentTraffic map[string]int
	deploymentLatency map[latencyKey]*latencyWindow
	capacityMinute    int64
	deploymentCapacity map[string]capacityUsage
	deploymentCooldown map[string]cooldownWindow
	polling *responsePolling
	mcpOAuthFlows mcpOAuthFlows
	mcpOAuthRefresh sync.Map
}

var credentialRefPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func NewServer(config Config) *Server {
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 8 << 20
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 128
	}
	if config.HealthCheckConcurrency <= 0 {
		config.HealthCheckConcurrency = 8
	}
	if config.ContainerOwners == nil && config.AuthorizeInference {
		config.ContainerOwners = &memoryContainerOwners{owners: make(map[containerKey]string)}
	}
	if config.InferenceCache.Enabled && config.InferenceCacheStore == nil {
		config.InferenceCacheStore = newMemoryInferenceCache()
	}
	routes := make(map[string]Route, len(config.Routes))
	for _, route := range config.Routes {
		routes[route.Alias] = route
	}
	server := &Server{config: config, routes: make(map[string]Route), configuredRoutes: routes,
		passThrough: config.PassThrough, agents: config.Agents,
		mcpServers: config.MCPServers,
		searchTools: config.SearchTools, searchFallbacks: config.SearchFallbacks, sem: make(chan struct{}, config.MaxConcurrent), veoReads: make(chan struct{}, 1)}
	if config.ResponsePolling.All || len(config.ResponsePolling.Providers) > 0 {
		server.polling = &responsePolling{entries: make(map[string]*pollingEntry)}
	}
	return server
}

// ReplaceConfiguredRouting publishes the model routes and search fallback rules together.
func (s *Server) ReplaceConfiguredRouting(routes []Route, searchTools []SearchTool, searchFallbacks map[string][]string) {
	next := make(map[string]Route, len(routes))
	for _, route := range routes {
		next[route.Alias] = route
	}
	s.mu.Lock()
	s.configuredRoutes = next
	s.searchTools = searchTools
	s.searchFallbacks = searchFallbacks
	s.healthResults = nil
	s.configGeneration++
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	if s.config.AuthorizeInference && s.serveInferenceCacheAdmin(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveAgent(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveHealth(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveSearch(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveMCPUserCredentials(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveMCPOAuth(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveMCPREST(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveMCPClientSSE(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.serveMCPEndpoint(writer, request) {
		return
	}
	if s.config.AuthorizeInference && s.servePassThrough(writer, request) {
		return
	}
	alias, geminiEndpoint := geminiMethod(request.URL.Path)
	var deployment, deploymentOperation string
	if s.config.AuthorizeInference && geminiEndpoint == "" {
		deployment, deploymentOperation = deploymentEndpoint(request.URL.Path)
		path := request.URL.Path
		azureResponses := path == "/openai/v1/responses" || strings.HasPrefix(path, "/openai/v1/responses/")
		anthropicMessages := path == "/anthropic/v1/messages" || strings.HasPrefix(path, "/anthropic/v1/messages/")
		if unversioned := proxyAlias(path); unversioned || azureResponses || anthropicMessages {
			normalized := *request.URL
			if unversioned {
				normalized.Path = "/v1" + path
				if normalized.RawPath != "" {
					normalized.RawPath = "/v1" + normalized.RawPath
				}
			} else {
				prefix := "/openai"
				if anthropicMessages {
					prefix = "/anthropic"
				}
				normalized.Path = strings.TrimPrefix(path, prefix)
				normalized.RawPath = strings.TrimPrefix(normalized.RawPath, prefix)
			}
			copy := request.WithContext(request.Context())
			copy.URL = &normalized
			request = copy
		}
	}
	interaction, isInteraction := interactionSuffix(request.URL.EscapedPath())
	var modelScope []string
	realtime := isRealtimeSocketPath(request.URL.Path)
	rtHTTP := realtimeHTTPOperation(request.URL.Path)
	if s.config.AuthorizeInference && request.Method == http.MethodPost && rtHTTP == "calls" {
		s.realtimeCall(writer, request)
		return
	}
	if s.config.AuthorizeInference && (isInteraction || realtime || rtHTTP != "" || deploymentOperation != "" || geminiEndpoint != "" ||
		request.URL.Path == "/utils/token_counter" || request.URL.Path == "/utils/transform_request" || request.URL.Path == "/model_group/info" ||
		strings.HasPrefix(request.URL.Path, "/v1/")) && !s.authorized(request) {
		allowed := false
		if s.config.AuthorizeKey != nil {
			modelScope, allowed = s.config.AuthorizeKey(ProxyCredential(request))
		}
		if !allowed {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return
		}
	}
	if s.config.AuthorizeInference && isInteraction {
		s.serveInteraction(writer, request, interaction, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && request.URL.Path == "/utils/transform_request" {
		s.transformPreview(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && request.URL.Path == "/utils/token_counter" {
		s.countTokens(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && request.URL.Path == "/v1/rag/query" {
		s.ragQuery(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && request.URL.Path == "/v1/rag/ingest" {
		s.ragIngest(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodGet && request.URL.Path == "/model_group/info" {
		s.modelGroups(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && rtHTTP == "client_secrets" {
		s.realtimeClientSecret(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && realtime && request.Method == http.MethodGet &&
		strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		s.realtimeSocket(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && strings.HasPrefix(request.URL.Path, responseResourcePrefix) &&
		request.URL.Path != endpoints["responses-compact"].path {
		if !s.pollingResource(writer, request, modelScope) {
			s.responseResource(writer, request, modelScope)
		}
		return
	}
	if s.config.AuthorizeInference && isVectorStorePath(request.URL.Path) {
		s.vectorStoreResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isContainerPath(request.URL.Path) {
		s.containerResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isFineTuningPath(request.URL.Path) {
		s.fineTuningResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isEvalPath(request.URL.Path) {
		s.evalResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isVideoPath(request.URL.Path) {
		s.videoResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isAnthropicSkillPath(request.URL.Path) {
		s.anthropicSkill(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isAnthropicBatchPath(request.URL.Path) {
		s.anthropicBatch(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && resourceFamily(request.URL.Path) != "" {
		s.openAIResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && request.Method == http.MethodGet &&
		request.URL.Path == "/v1/responses" && strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		s.responsesSocket(writer, request, modelScope)
		return
	}
	if request.Method == http.MethodPost {
		if deploymentOperation != "" {
			s.infer(writer, request, deploymentOperation, deployment, modelScope)
			return
		}
		if geminiEndpoint != "" {
			s.infer(writer, request, geminiEndpoint, alias, modelScope)
			return
		}
		endpoint := EndpointFromPath(request.URL.Path)
		if endpoint != "" {
			s.infer(writer, request, endpoint, "", modelScope)
			return
		}
		if request.URL.Path == "/v1/messages/count_tokens" {
			s.infer(writer, request, "anthropic-count-tokens", "", modelScope)
			return
		}
	}
	switch {
	case request.Method == http.MethodGet && (request.URL.Path == "/healthz" || request.URL.Path == "/health/liveliness"):
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		s.models(writer, request, modelScope)
	case s.config.AuthorizeInference && request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/models/"):
		s.modelInfo(writer, strings.TrimPrefix(request.URL.Path, "/v1/models/"), modelScope)
	case strings.HasPrefix(request.URL.Path, "/internal/v1/routes/"):
		s.route(writer, request)
	default:
		if !s.config.AuthorizeInference || !s.serveProviderPassThrough(writer, request) {
			writeError(writer, http.StatusNotFound, "not_found", "Route not found")
		}
	}
}

func (s *Server) models(writer http.ResponseWriter, request *http.Request, modelScope []string) {
	query := request.URL.Query()
	fallbackType := query.Get("fallback_type")
	includeMetadata := s.config.AuthorizeInference && query.Get("include_metadata") == "true"
	if includeMetadata && fallbackType != "" && fallbackType != "general" &&
		fallbackType != "context_window" && fallbackType != "content_policy" {
		writeError(writer, http.StatusBadRequest, "invalid_fallback_type", "Invalid fallback type")
		return
	}
	if s.config.AuthorizeInference && query.Get("scope") != "" && query.Get("scope") != "expand" {
		writeError(writer, http.StatusBadRequest, "invalid_scope", "Invalid model scope")
		return
	}
	s.mu.RLock()
	data := make([]map[string]any, 0, len(s.routes)+len(s.configuredRoutes))
	for alias, route := range s.routes {
		if modelAllowed(modelScope, alias) {
			data = append(data, s.modelEntry(route, includeMetadata, fallbackType, modelScope))
		}
	}
	for alias, route := range s.configuredRoutes {
		if _, overridden := s.routes[alias]; !overridden && !route.Hidden && modelAllowed(modelScope, alias) {
			data = append(data, s.modelEntry(route, includeMetadata, fallbackType, modelScope))
		}
	}
	s.mu.RUnlock()
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) modelInfo(writer http.ResponseWriter, alias string, scope []string) {
	s.mu.RLock()
	route, ok := s.routes[alias]
	if !ok {
		route, ok = s.configuredRoutes[alias]
	}
	s.mu.RUnlock()
	if !ok || route.Hidden || !modelAllowed(scope, alias) {
		writeError(writer, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	writeJSON(writer, http.StatusOK, s.modelEntry(route, false, "", scope))
}

func (s *Server) modelEntry(route Route, metadata bool, fallbackType string, scope []string) map[string]any {
	entry := map[string]any{"id": route.Alias, "object": "model", "owned_by": "minato-runtime"}
	if s.config.AuthorizeInference {
		entry["created"] = 1677610602
		if len(route.Candidates) > 0 && route.Candidates[0].Provider != "" {
			entry["owned_by"] = route.Candidates[0].Provider
		}
	}
	if metadata {
		candidates := route.Candidates
		switch fallbackType {
		case "context_window":
			candidates = route.ContextFallback
		case "content_policy":
			candidates = route.PolicyFallback
		}
		fallbacks := make([]string, 0)
		for _, candidate := range candidates {
			name := candidate.RoutingGroup
			if name != "" && name != route.Alias && modelAllowed(scope, name) && !slices.Contains(fallbacks, name) {
				fallbacks = append(fallbacks, name)
			}
		}
		entry["metadata"] = map[string]any{"fallbacks": fallbacks}
	}
	return entry
}

func (s *Server) route(writer http.ResponseWriter, request *http.Request) {
	if !s.authorized(request) {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "Runtime token required")
		return
	}
	alias := strings.TrimPrefix(request.URL.Path, "/internal/v1/routes/")
	if alias == "" || strings.Contains(alias, "/") {
		writeError(writer, http.StatusBadRequest, "invalid_alias", "Invalid route alias")
		return
	}
	if request.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.routes, alias)
		s.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if request.Method != http.MethodPut {
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	var route Route
	if err := decodeBody(request, &route, s.config.MaxBodyBytes); err != nil || route.Alias != alias || route.Version == "" || len(route.Candidates) == 0 || len(route.Candidates) > 16 {
		writeError(writer, http.StatusBadRequest, "invalid_route", "A route alias, version and candidates are required")
		return
	}
	for _, candidate := range route.Candidates {
		if candidate.APIBase == "" || candidate.ModelID == "" || candidate.Endpoint == "" {
			writeError(writer, http.StatusBadRequest, "invalid_candidate", "Every candidate needs an API base, model ID and endpoint")
			return
		}
		if _, ok := endpoints[candidate.Endpoint]; !ok {
			writeError(writer, http.StatusBadRequest, "invalid_endpoint", "Unsupported candidate endpoint")
			return
		}
		if !CompatibleEndpoints(candidate.Endpoint, route.Candidates[0].Endpoint) {
			writeError(writer, http.StatusBadRequest, "invalid_endpoint", "Route candidates must support a compatible endpoint")
			return
		}
		if candidate.CredentialRef != "" && !credentialRefPattern.MatchString(candidate.CredentialRef) {
			writeError(writer, http.StatusBadRequest, "invalid_credential_ref", "Credential reference must be an environment variable name")
			return
		}
	}
	s.mu.Lock()
	s.routes[alias] = route
	s.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{"alias": alias, "version": route.Version})
}

func (s *Server) infer(writer http.ResponseWriter, request *http.Request, endpoint, alias string, modelScope []string) {
	body, err := readBody(request, s.config.MaxBodyBytes)
	if err != nil {
		writeError(writer, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		return
	}
	payload, err := decodePayload(request.Header.Get("Content-Type"), body, endpoint)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input := payload.fields
	stream := endpoint == "gemini-stream" || request.Header.Get("Accept") == "text/event-stream" || inputBool(input, "stream")
	if alias == "" {
		alias, _ = input["model"].(string)
	}
	customer := requestCustomerID(request, payload)
	if s.config.Callback != nil {
		recorder := &callbackResponse{ResponseWriter: writer, event: newInferenceEvent(endpoint, alias, stream)}
		recorder.event.EndUser = customer
		if key := ProxyCredential(request); key != "" && !s.authorized(request) {
			recorder.event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		}
		if s.config.LogMessages {
			recorder.capture = true
			for _, name := range []string{"messages", "input", "prompt", "contents"} {
				if value := input[name]; value != nil {
					recorder.event.Messages = boundedCallbackJSON(value)
					break
				}
			}
		}
		defer func() {
			if recorder.status != 0 {
				recorder.finish()
				s.config.Callback(recorder.event)
			}
		}()
		writer = recorder
	}
	if !modelAllowed(modelScope, alias) {
		writeError(writer, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	var customerPolicy CustomerPolicy
	if customer != "" && s.config.CustomerPolicy != nil {
		var err error
		customerPolicy, err = s.config.CustomerPolicy(request.Context(), customer)
		if err != nil {
			writeError(writer, http.StatusServiceUnavailable, "customer_lookup_failed", "Could not read customer access")
			return
		}
		if customerPolicy.Blocked {
			writeError(writer, http.StatusForbidden, "customer_blocked", "This customer is blocked")
			return
		}
	}
	s.mu.RLock()
	route, ok := s.routes[alias]
	if !ok {
		route, ok = s.configuredRoutes[alias]
	}
	s.mu.RUnlock()
	if !ok {
		writer.Header().Set("X-Minato-Runtime-Route-Missing", "1")
		writeError(writer, http.StatusNotFound, "model_not_found", "Unknown runtime route")
		return
	}
	if s.config.AuthorizeInference {
		var err error
		route, err = s.requestFallbacks(route, input, modelScope, endpoint)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_fallback", err.Error())
			return
		}
	}
	eligible := s.eligibleCandidates(request.Context(), route.Candidates, modelScope, endpoint, stream)
	if customerPolicy.Region != "" {
		eligible = customerRegionCandidates(eligible, customerPolicy.Region)
		if len(eligible) == 0 && customerPolicy.DefaultModel != "" && modelAllowed(modelScope, customerPolicy.DefaultModel) {
			s.mu.RLock()
			regional := s.configuredRoutes[customerPolicy.DefaultModel]
			s.mu.RUnlock()
			eligible = customerRegionCandidates(s.eligibleCandidates(request.Context(), regional.Candidates, modelScope, endpoint, stream), customerPolicy.Region)
		}
		if len(eligible) == 0 {
			writeError(writer, http.StatusBadRequest, "model_region_unavailable", "No deployment matches the customer's allowed region")
			return
		}
	}
	if s.config.EnableTagFiltering {
		before := len(eligible)
		eligible = filterTaggedDeployments(eligible, input, request.UserAgent(), s.config.TagsMatchAll)
		if before > 0 && len(eligible) == 0 {
			writeError(writer, http.StatusBadRequest, "no_deployments_with_tag_routing", "No deployments match the request tags")
			return
		}
	}
	if len(eligible) == 0 {
		writeError(writer, http.StatusBadRequest, "unsupported_endpoint", "Model does not support this endpoint")
		return
	}
	route.Candidates = eligible
	route.ContextFallback = s.eligibleCandidates(request.Context(), route.ContextFallback, modelScope, endpoint, stream)
	route.PolicyFallback = s.eligibleCandidates(request.Context(), route.PolicyFallback, modelScope, endpoint, stream)
	route.ContextFallback = customerRegionCandidates(route.ContextFallback, customerPolicy.Region)
	route.PolicyFallback = customerRegionCandidates(route.PolicyFallback, customerPolicy.Region)
	if s.config.EnableTagFiltering {
		if route.ContextFallback != nil {
			route.ContextFallback = filterTaggedDeployments(route.ContextFallback, input, request.UserAgent(), s.config.TagsMatchAll)
		}
		if route.PolicyFallback != nil {
			route.PolicyFallback = filterTaggedDeployments(route.PolicyFallback, input, request.UserAgent(), s.config.TagsMatchAll)
		}
	}
	if s.config.AuthorizeInference && (endpoint == "responses" || endpoint == "chat-completions") {
		automatic := make(map[string]mcpInferenceTool)
		if err := s.expandMCPInferenceTools(request, endpoint, input, automatic); err != nil {
			writeError(writer, http.StatusBadRequest, "mcp_tool_error", err.Error())
			return
		}
		if len(automatic) > 0 {
			if endpoint == "responses" && s.shouldPollResponse(route, input) {
				s.startResponsePolling(writer, request, route.Alias, body)
				return
			}
			if recorder, ok := writer.(*callbackResponse); ok {
				writer = recorder.ResponseWriter
			}
			s.inferWithMCPTools(writer, request, endpoint, alias, modelScope, input, automatic, stream)
			return
		}
	}
	if s.config.AuthorizeInference && endpoint == "anthropic-messages" {
		if err := pinAnthropicResources(&route, input); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_resource_reference", err.Error())
			return
		}
	}
	if s.config.AuthorizeInference && endpoint == "responses" {
		var originalPollingPayload []byte
		if s.shouldPollResponse(route, input) {
			originalPollingPayload = body
		}
		previous, _ := input["previous_response_id"].(string)
		if strings.HasPrefix(previous, pollingIDPrefix) {
			previous = s.resolvePollingParent(request, previous, modelScope)
			if previous == "" {
				writeError(writer, http.StatusNotFound, "response_not_found", "Unknown or incomplete polling response")
				return
			}
			input["previous_response_id"] = previous
		}
		provider, deployment, original, wrapped := unwrapResponseID(previous)
		if wrapped {
			input["previous_response_id"] = original
		} else {
			provider, deployment = "", ""
		}
		if err := s.rewriteResponseVectorStores(request, input, route.Alias, modelScope, &deployment); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		containers, err := rewriteResponseContainers(input, &deployment)
		if err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if deployment != "" && !route.pinResponseDeployment(provider, deployment) {
			writeError(writer, http.StatusBadRequest, "response_not_found", "Response resources do not belong to this route")
			return
		}
		if status, message := s.authorizeResponseContainers(request, &route, deployment, containers); status != 0 {
			writeError(writer, status, "container_error", message)
			return
		}
		if originalPollingPayload != nil {
			s.startResponsePolling(writer, request, route.Alias, originalPollingPayload)
			return
		}
	}
	cache := s.prepareInferenceCache(request, endpoint, route.Alias, stream, &payload)
	if stream {
		if !endpoints[endpoint].streaming {
			writeError(writer, http.StatusBadRequest, "streaming_unsupported", "This endpoint does not support event streams")
			return
		}
		s.inferStream(writer, request, endpoint, route, payload, cache)
		return
	}
	attempts := make([]map[string]any, 0, len(route.Candidates))
	plan := s.attemptPlan(route.Alias)
	for {
		candidate := route.Candidates[plan.index]
		cacheKey := cache.key(candidate)
		if cacheKey != "" && s.serveCachedInference(writer, request, cache, cacheKey, candidate, attempts) {
			return
		}
		started := time.Now()
		reservation, admitted := s.admitDeployment(request.Context(), candidate, payload)
		var status int
		var responseBody []byte
		var responseHeaders http.Header
		var retryable bool
		var callErr error
		if admitted {
			status, responseBody, responseHeaders, retryable, callErr = s.callJSON(request.Context(), request, endpoint, candidate, payload)
			s.recordCooldown(request.Context(), candidate, status, retryable, responseHeaders)
			if reservation != nil && status >= 200 && status < 300 {
				s.settleCapacity(request.Context(), reservation, usageFromJSON(responseBody))
			}
		} else {
			status, responseBody, responseHeaders = capacityError()
			retryable = plan.index+1 < len(route.Candidates)
		}
		attempt := map[string]any{"id": newUUID(), "sourceId": candidate.SourceID, "modelId": candidate.ModelID,
			"status": status, "durationMs": time.Since(started).Milliseconds(), "retryable": retryable}
		if callErr != nil {
			attempt["status"] = http.StatusBadGateway
		}
		if status >= 200 && status < 300 {
			var payload map[string]any
			if json.Unmarshal(responseBody, &payload) == nil {
				attempt["usage"] = payload["usage"]
				if isGeminiEndpoint(candidate.Endpoint) {
					attempt["usage"] = geminiUsage(payload)
				}
				if (candidate.Endpoint == "anthropic-messages" || bedrockClaudeMessages(endpoint, candidate)) && endpoint != "anthropic-count-tokens" {
					usage, _ := payload["usage"].(map[string]any)
					attempt["usage"] = anthropicUsage(usage)
				}
				if candidate.Endpoint == "bedrock-converse" && endpoint != "anthropic-messages" {
					attempt["usage"] = bedrockUsage(payload["usage"])
				}
			}
		}
		attempts = append(attempts, attempt)
		if callErr == nil && status >= 200 && status < 300 {
			s.recordLatency(request.Context(), candidate, false, time.Since(started))
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			body := normalizeResponse(endpoint, candidate.Endpoint, responseBody, route.Alias)
			if endpoint == "ocr" && usesMistralOCR(candidate.Provider) {
				if isDocumentIntelligence(candidate) {
					body = azureDocumentOCRResponse(responseBody, route.Alias)
				} else if isVertexDeepSeekOCR(candidate) {
					body = deepSeekOCRResponse(responseBody, route.Alias)
				} else {
					body = mistralOCRResponse(responseBody, route.Alias)
				}
			}
			if endpoint == "rerank" && candidate.Provider == "voyage" {
				body = voyageRerankResponse(body)
			}
			if endpoint == "rerank" {
				switch candidate.Provider {
				case "fireworks_ai", "hosted_vllm":
					body = hostedRerankResponse(body, candidate.Provider)
				case "deepinfra":
					body = deepInfraRerankResponse(body)
				case "nvidia_nim":
					body = nvidiaRerankResponse(body, payload.fields)
				case "huggingface":
					body = huggingFaceRerankResponse(body, payload.fields, candidate.ModelID)
				case "watsonx":
					body = watsonXRerankResponse(body)
				case "infinity", "together_ai":
					body = cohereStyleRerankResponse(body, candidate.Provider)
				}
			}
			if endpoint == "rerank" && candidate.Vertex != nil {
				body = vertexRerankResponse(body)
			}
			if endpoint == "rerank" && candidate.Bedrock != nil {
				body = bedrockRerankResponse(body)
			}
			if s.config.AuthorizeInference && endpoint == "responses" && candidate.Endpoint == "responses" && candidate.DeploymentID != "" {
				body = wrapResponseBody(body, candidate, s.responseContainerEncoder(request, candidate))
			}
			if candidate.Endpoint == "bedrock-embeddings" && strings.HasPrefix(candidate.ModelID, "async_invoke/") {
				if arn := responseHeaders.Get("X-LiteLLM-Invocation-ARN"); arn != "" {
					writer.Header().Set("X-LiteLLM-Invocation-ARN", arn)
				}
			}
			if cacheKey != "" {
				s.storeInference(request, cache, cacheKey, status, responseHeaders.Get("Content-Type"), body)
			}
			writeResponse(writer, status, responseHeaders, body)
			return
		}
		if !plan.next(request.Context(), status, responseHeaders, retryable, responseBody, &route, admitted) {
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			if callErr != nil {
				writeError(writer, http.StatusBadGateway, "upstream_error", callErr.Error())
			} else {
				writeRawResponse(writer, status, responseHeaders, responseBody)
			}
			return
		}
	}
}

func (s *Server) inferStream(writer http.ResponseWriter, request *http.Request, endpoint string, route Route, payload requestPayload, cache inferenceCacheRequest) {
	attempts := make([]map[string]any, 0, len(route.Candidates))
	plan := s.attemptPlan(route.Alias)
	for {
		candidate := route.Candidates[plan.index]
		cacheKey := cache.key(candidate)
		if cacheKey != "" && s.serveCachedInference(writer, request, cache, cacheKey, candidate, attempts) {
			return
		}
		started := time.Now()
		reservation, admitted := s.admitDeployment(request.Context(), candidate, payload)
		var status int
		var response *http.Response
		var retryable bool
		var err error
		if admitted {
			status, response, retryable, err = s.callStream(request.Context(), request, endpoint, candidate, payload)
		} else {
			code, body, headers := capacityError()
			status = code
			response = &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(bytes.NewReader(body))}
		}
		var reader *bufio.Reader
		var firstFrame []byte
		var errorBody []byte
		streamFailure := false
		if err == nil && response != nil && status >= 200 && status < 300 {
			reader = bufio.NewReader(response.Body)
			for {
				firstFrame, err = readSSEFrame(reader, s.config.MaxBodyBytes)
				if err != nil || bytes.HasPrefix(firstFrame, []byte("data:")) || bytes.Contains(firstFrame, []byte("\ndata:")) {
					break
				}
			}
			if err != nil || sseFrameFailure(firstFrame) {
				errorBody = streamErrorBody(firstFrame)
				streamFailure = true
				retryable = true
				status = http.StatusBadGateway
				response.Body.Close()
			}
		}
		if err == nil && !streamFailure && response != nil && (status < 200 || status >= 300) {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes))
			errorBody = body
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(body))
			if readErr != nil {
				retryable = true
			} else {
				retryable = (!admitted && plan.index+1 < len(route.Candidates)) || (admitted && retryableResponse(status, body))
			}
		}
		if admitted {
			var headers http.Header
			if response != nil {
				headers = response.Header
			}
			s.recordCooldown(request.Context(), candidate, status, retryable, headers)
		}
		attempt := map[string]any{"id": newUUID(), "sourceId": candidate.SourceID, "modelId": candidate.ModelID,
			"status": status, "durationMs": time.Since(started).Milliseconds(), "retryable": retryable}
		if err != nil {
			attempt["status"] = http.StatusBadGateway
		}
		attempts = append(attempts, attempt)
		if err == nil && !streamFailure && status >= 200 && status < 300 {
			s.recordLatency(request.Context(), candidate, true, time.Since(started))
			defer response.Body.Close()
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			for key, values := range response.Header {
				if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Cache-Control") {
					writer.Header()[key] = values
				}
			}
			writer.Header().Set("Cache-Control", "no-cache, no-transform")
			writer.Header().Set("X-Accel-Buffering", "no")
			writer.WriteHeader(status)
			flush, _ := writer.(http.Flusher)
			convert := streamConverter(endpoint, candidate.Endpoint, route.Alias, payload.fields, s.config.MaxBodyBytes)
			if s.config.AuthorizeInference && endpoint == "responses" && candidate.Endpoint == "responses" && candidate.DeploymentID != "" {
				encode := s.responseContainerEncoder(request, candidate)
				convert = func(frame []byte) []byte { return wrapResponseEvent(frame, candidate, encode) }
			}
			if reservation != nil {
				baseConvert := convert
				convert = func(frame []byte) []byte {
					s.settleCapacity(request.Context(), reservation, usageFromJSON(streamErrorBody(frame)))
					return baseConvert(frame)
				}
			}
			if recorder, ok := writer.(*callbackResponse); ok &&
				(candidate.Endpoint == "bedrock-converse" || isGeminiEndpoint(candidate.Endpoint) ||
					(candidate.Endpoint == "anthropic-messages" && endpoint != candidate.Endpoint)) {
				baseConvert := convert
				convert = func(frame []byte) []byte {
					if raw := streamErrorBody(frame); len(raw) != 0 {
						recorder.recordJSON(raw)
					}
					return baseConvert(frame)
				}
			}
			capture := streamCacheCapture{responses: cache.responses, disabled: cacheKey == "" || !cache.write || cache.ttl <= 0}
			emit := func(frame []byte) bool {
				output := convert(frame)
				capture.add(output)
				return writeStreamFrame(writer, flush, output)
			}
			if len(firstFrame) > 0 && !emit(firstFrame) {
				return
			}
			for {
				frame, readErr := readSSEFrame(reader, s.config.MaxBodyBytes)
				if len(frame) > 0 && !emit(frame) {
					return
				}
				if readErr != nil {
					if readErr == io.EOF && isGeminiEndpoint(candidate.Endpoint) && isTextEndpoint(endpoint) {
						if !emit(nil) {
							return
						}
					}
					if readErr == io.EOF && capture.complete() {
						s.storeInference(request, cache, cacheKey, status, "text/event-stream", capture.frames)
					}
					break
				}
			}
			return
		}
		var headers http.Header
		if response != nil {
			headers = response.Header
		}
		if !plan.next(request.Context(), status, headers, retryable, errorBody, &route, admitted) {
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			if streamFailure {
				writeError(writer, http.StatusBadGateway, "upstream_stream_failure", "Upstream stream failed before data")
				return
			}
			if err != nil {
				writeError(writer, http.StatusBadGateway, "upstream_error", err.Error())
			} else {
				body, _ := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes))
				response.Body.Close()
				writeRawResponse(writer, status, response.Header, body)
			}
			return
		}
		if response != nil {
			response.Body.Close()
		}
	}
}

func (s *Server) eligibleCandidates(ctx context.Context, candidates []Candidate, scope []string, endpoint string, stream bool) []Candidate {
	if candidates == nil {
		return nil
	}
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if endpoint == "anthropic-messages" && candidate.Endpoint == "bedrock-converse" && !bedrockClaudeMessages(endpoint, candidate) {
			continue
		}
		if candidateAllowed(scope, candidate) && servesEndpoint(endpoint, candidate.Endpoint) && (!stream || (!googleImageDeployment(candidate) && !nativeAudioDeployment(candidate) && !multipartAudioDeployment(candidate))) {
			if candidate.DeploymentID != "" {
				index := slices.IndexFunc(eligible, func(existing Candidate) bool {
					return existing.DeploymentID == candidate.DeploymentID && existing.RoutingGroup == candidate.RoutingGroup
				})
				if index >= 0 {
					if candidate.Endpoint == endpoint {
						eligible[index] = candidate
					}
					continue
				}
			}
			eligible = append(eligible, candidate)
		}
	}
	s.orderDeployments(ctx, eligible, stream)
	return s.filterUnhealthyCandidates(eligible)
}

func writeStreamFrame(writer http.ResponseWriter, flush http.Flusher, frame []byte) bool {
	if len(frame) == 0 {
		return true
	}
	if _, err := writer.Write(frame); err != nil {
		return false
	}
	if flush != nil {
		flush.Flush()
	}
	return true
}

func streamConverter(endpoint, upstream, alias string, input map[string]any, limit int64) func([]byte) []byte {
	if isGeminiEndpoint(upstream) && isTextEndpoint(endpoint) {
		options, _ := input["stream_options"].(map[string]any)
		bridge := newGeminiChatStream(alias, endpoint == "responses" || inputBool(options, "include_usage"))
		if endpoint == "responses" {
			responses := &chatResponsesStream{alias: alias, limit: limit}
			return func(frame []byte) []byte { return responses.convert(bridge.convert(frame)) }
		}
		return bridge.convert
	}
	if (upstream == "anthropic-messages" || upstream == "bedrock-converse") && isTextEndpoint(endpoint) {
		options, _ := input["stream_options"].(map[string]any)
		bridge := newAnthropicChatStream(alias, endpoint == "responses" || inputBool(options, "include_usage"))
		if endpoint == "responses" {
			responses := &chatResponsesStream{alias: alias, limit: limit}
			return func(frame []byte) []byte { return responses.convert(bridge.convert(frame)) }
		}
		return bridge.convert
	}
	if endpoint == "responses" && upstream == "chat-completions" {
		bridge := &chatResponsesStream{alias: alias, limit: limit}
		return bridge.convert
	}
	if endpoint == "chat-completions" && upstream == "responses" {
		options, _ := input["stream_options"].(map[string]any)
		bridge := &responsesChatStream{alias: alias, includeUsage: inputBool(options, "include_usage")}
		return bridge.convert
	}
	return func(frame []byte) []byte { return frame }
}

func (s *Server) callJSON(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload) (int, []byte, http.Header, bool, error) {
	if candidate.Endpoint == "bedrock-converse" && endpoint == "anthropic-count-tokens" {
		raw, _ := payload.fields["messages"].([]any)
		messages := make([]map[string]any, 0, len(raw))
		for _, value := range raw {
			if message, ok := value.(map[string]any); ok {
				messages = append(messages, message)
			}
		}
		if len(messages) == 0 || len(messages) != len(raw) {
			return http.StatusBadRequest, []byte(`{"error":{"message":"Expected a list of messages"}}`),
				http.Header{"Content-Type": {"application/json"}}, false, nil
		}
		tools, _ := payload.fields["tools"].([]any)
		return s.countBedrockInput(ctx, request, candidate, bedrockTokenCountPayload(messages, payload.fields["system"], tools))
	}
	if candidate.Endpoint == "bedrock-embeddings" {
		return s.bedrockEmbeddings(ctx, request, candidate, payload)
	}
	if candidate.Endpoint == "google-embeddings" {
		return s.googleEmbeddings(ctx, request, candidate, payload)
	}
	status, data, headers, retryable, err := s.callProviderJSON(ctx, request, endpoint, candidate, payload)
	if err == nil && status/100 == 2 && multipartAudioDeployment(candidate) {
		data, err = multipartAudioResponse(data, headers.Get("Content-Type"), candidate.Provider)
		headers.Set("Content-Type", "application/json")
	}
	if err == nil && status/100 == 2 && nativeAudioDeployment(candidate) && candidate.Endpoint == "transcription" {
		data, err = providerTranscription(data, candidate.Provider)
		if err != nil {
			return http.StatusBadGateway, nil, headers, true, errors.New("Invalid provider transcription response")
		}
	}
	if err == nil && status/100 == 2 && vertexSpeechDeployment(candidate) {
		data, err = vertexSpeechAudio(data)
		if err != nil {
			return http.StatusBadGateway, nil, headers, true, errors.New("Invalid Vertex speech audio response")
		}
		_, media, _ := vertexSpeechPayload(payload.fields)
		headers.Set("Content-Type", media)
	}
	if err == nil && status/100 == 2 && googleImageDeployment(candidate) {
		data, err = googleImageResponse(data)
		if err != nil {
			return http.StatusBadGateway, nil, headers, true, err
		}
	}
	if err == nil && status/100 == 2 && bedrockImageDeployment(candidate) {
		data, err = bedrockImageResponse(data)
		if err != nil {
			var rejected bedrockImageRejection
			if errors.As(err, &rejected) {
				body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "image_generation_failed", "message": rejected.Error()}})
				return http.StatusBadRequest, body, headers, false, nil
			}
			return http.StatusBadGateway, nil, headers, true, err
		}
	}
	return status, data, headers, retryable, err
}

func (s *Server) callProviderJSON(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload) (int, []byte, http.Header, bool, error) {
	response, err := s.send(ctx, request, endpoint, candidate, payload, false)
	if err != nil {
		var invalid invalidMediaRequest
		if errors.As(err, &invalid) {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": invalid.Error(), "type": "invalid_request"}})
			return http.StatusBadRequest, body, http.Header{"Content-Type": {"application/json"}}, false, nil
		}
		return 0, nil, nil, true, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if int64(len(body)) > s.config.MaxBodyBytes {
		return response.StatusCode, nil, response.Header, false, errors.New("upstream response exceeds the body limit")
	}
	if err != nil {
		return response.StatusCode, nil, response.Header, true, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, body, response.Header, false, nil
	}
	return response.StatusCode, body, response.Header, retryableResponse(response.StatusCode, body), nil
}

func (s *Server) callStream(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload) (int, *http.Response, bool, error) {
	response, err := s.send(ctx, request, endpoint, candidate, payload, true)
	if err != nil {
		return 0, nil, true, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response.StatusCode, response, false, nil
	}
	return response.StatusCode, response, retryableStatus(response.StatusCode), nil
}

func encodePayload(input requestPayload, endpoint string, candidate Candidate, stream bool) ([]byte, string, error) {
	if endpoint == "bedrock-count-tokens" {
		body, err := json.Marshal(input.fields)
		return body, "application/json", err
	}
	if bedrockImageDeployment(candidate) {
		payload, err := bedrockImagePayload(input, candidate)
		if err != nil {
			return nil, "", invalidMediaRequest{err}
		}
		body, err := json.Marshal(payload)
		return body, "application/json", err
	}
	if googleImageDeployment(candidate) {
		var payload map[string]any
		var err error
		if candidate.Endpoint == "image-edit" {
			payload, err = googleImageEditPayload(input, candidate)
		} else {
			payload, err = googleImagePayload(input.fields, candidate)
		}
		if err != nil {
			return nil, "", invalidMediaRequest{err}
		}
		body, err := json.Marshal(payload)
		return body, "application/json", err
	}
	if candidate.Endpoint == "bedrock-embeddings" || candidate.Endpoint == "google-embeddings" {
		body, err := json.Marshal(input.fields)
		return body, "application/json", err
	}
	if input.form != nil {
		return input.encodeMultipart(candidate.ModelID, stream)
	}
	targetEndpoint := candidate.Endpoint
	payload := clone(input.fields)
	if isGeminiEndpoint(targetEndpoint) {
		delete(payload, "model")
		delete(payload, "stream")
	} else {
		payload["model"] = candidate.ModelID
	}
	if endpoints[targetEndpoint].streaming && endpoint != "anthropic-count-tokens" && !isGeminiEndpoint(targetEndpoint) {
		payload["stream"] = stream
	}
	if endpoint == "responses-compact" {
		delete(payload, "stream")
	}
	if endpoint == "responses" && targetEndpoint == "chat-completions" {
		payload = responsesToChat(payload)
		if stream {
			payload["stream_options"] = map[string]any{"include_usage": true}
		}
	} else if endpoint == "chat-completions" && targetEndpoint == "responses" {
		payload = chatRequestToResponses(payload)
	}
	if targetEndpoint == "anthropic-messages" && isTextEndpoint(endpoint) {
		if endpoint == "responses" {
			payload = responsesToChat(payload)
		}
		payload = chatToAnthropicRequest(payload)
	}
	if isGeminiEndpoint(targetEndpoint) && isTextEndpoint(endpoint) {
		if endpoint == "responses" {
			payload = responsesToChat(payload)
		}
		payload = chatToGemini(payload)
	}
	if targetEndpoint == "bedrock-converse" {
		if endpoint == "responses" {
			payload = responsesToChat(payload)
		}
		payload = chatToBedrock(payload)
	}
	if targetEndpoint == "chat-completions" {
		applyProviderChatSettings(payload, candidate)
	}
	if targetEndpoint == "rerank" && candidate.Provider == "jina_ai" {
		for key := range payload {
			switch key {
			case "model", "query", "documents", "top_n", "return_documents":
			default:
				delete(payload, key)
			}
		}
	}
	if targetEndpoint == "rerank" && candidate.Provider == "voyage" {
		payload = voyageRerankRequest(payload)
	}
	if targetEndpoint == "rerank" {
		switch candidate.Provider {
		case "fireworks_ai", "hosted_vllm", "together_ai":
			payload = hostedRerankRequest(payload, candidate.Provider)
		case "deepinfra":
			payload = deepInfraRerankRequest(payload)
		case "nvidia_nim":
			payload = nvidiaRerankRequest(payload)
		case "huggingface":
			payload = huggingFaceRerankRequest(payload)
		case "watsonx":
			payload, err := watsonXRerankRequest(payload, candidate)
			if err != nil {
				return nil, "", err
			}
			body, err := json.Marshal(payload)
			return body, "application/json", err
		case "infinity", "azure_ai":
			payload = hostedRerankRequest(payload, candidate.Provider)
			if input.fields["max_chunks_per_doc"] != nil {
				payload["max_chunks_per_doc"] = input.fields["max_chunks_per_doc"]
			}
		}
	}
	if targetEndpoint == "rerank" && candidate.Vertex != nil {
		payload = vertexRerankRequest(payload)
	}
	if targetEndpoint == "rerank" && candidate.Bedrock != nil {
		payload = bedrockRerankRequest(payload, candidate.Bedrock.Region)
	}
	if targetEndpoint == "cohere-embeddings" {
		payload = cohereEmbeddingRequest(payload)
	}
	if targetEndpoint == "ocr" && usesMistralOCR(candidate.Provider) {
		if isDocumentIntelligence(candidate) {
			payload = azureDocumentOCRRequest(payload)
		} else if isVertexDeepSeekOCR(candidate) {
			payload = deepSeekOCRRequest(payload)
		} else {
			payload = mistralOCRRequest(payload)
		}
	}
	body, err := json.Marshal(payload)
	return body, "application/json", err
}

func (s *Server) send(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload, stream bool) (*http.Response, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	preview, _ := ctx.Value(previewContextKey{}).(*rawRequestPreview)
	finished := func() {}
	if preview == nil {
		finished = s.trackDeployment(ctx, candidate)
	}
	release := func() {
		finished()
		<-s.sem
	}
	azureDocument := candidate.Endpoint == "ocr" && isDocumentIntelligence(candidate)
	if candidate.Endpoint == "ocr" && (candidate.Provider == "azure_ai" || candidate.Vertex != nil) &&
		!isVertexDeepSeekOCR(candidate) && !azureDocument {
		fields, err := preparePartnerOCR(ctx, payload.fields, s.config.MaxBodyBytes)
		if err != nil {
			release()
			return nil, err
		}
		payload.fields = fields
	}
	var encoded []byte
	var contentType, audioTarget, speechFormat string
	var err error
	invokeClaude := bedrockClaudeMessages(endpoint, candidate)
	if invokeClaude {
		encoded, err = bedrockClaudePayload(payload.fields, candidate.ModelID, request.Header.Values("anthropic-beta"))
		contentType = "application/json"
	} else if candidate.Provider == "aws_polly" {
		audioTarget = strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1") + "/v1/speech"
		encoded, err = pollyPayload(payload.fields, candidate.ModelID)
		if err != nil { err = invalidMediaRequest{err} }
		contentType = "application/json"
	} else if vertexSpeechDeployment(candidate) {
		audioTarget = candidate.APIBase
		encoded, _, err = vertexSpeechPayload(payload.fields)
		if err != nil { err = invalidMediaRequest{err} }
		contentType = "application/json"
	} else if azureSpeechDeployment(candidate) {
		audioTarget, err = azureSpeechTarget(candidate.APIBase)
		if err == nil {
			encoded, speechFormat, err = azureSpeechRequest(payload.fields)
			if err != nil { err = invalidMediaRequest{err} }
		}
		contentType = "application/ssml+xml"
	} else if multipartAudioDeployment(candidate) {
		audioTarget, encoded, contentType, err = multipartAudioRequest(payload, candidate)
		if err != nil {
			err = invalidMediaRequest{err}
		}
	} else if nativeAudioDeployment(candidate) {
		audioTarget, encoded, contentType, err = audioProviderRequest(payload, candidate)
		if err != nil { err = invalidMediaRequest{err} }
	} else {
		encoded, contentType, err = encodePayload(payload, endpoint, candidate, stream)
	}
	if err != nil {
		release()
		return nil, err
	}
	pathEndpoint := candidate.Endpoint
	if endpoint == "responses-compact" {
		pathEndpoint = endpoint
	}
	path := endpoints[pathEndpoint].path
	if endpoint == "anthropic-count-tokens" {
		path = "/v1/messages/count_tokens"
	}
	base := strings.TrimRight(candidate.APIBase, "/")
	if strings.HasSuffix(base, "/v1") {
		base = strings.TrimSuffix(base, "/v1")
	}
	target := base + path
	if candidate.RelativeAPIBase {
		target = strings.TrimRight(candidate.APIBase, "/") + strings.TrimPrefix(path, "/v1")
	}
	if audioTarget != "" {
		target = audioTarget
	} else if googleImageDeployment(candidate) {
		target = googleImageTarget(candidate)
	} else if bedrockImageDeployment(candidate) {
		target = strings.TrimRight(candidate.APIBase, "/") + "/model/" + url.PathEscape(candidate.ModelID) + "/invoke"
	} else if isGeminiEndpoint(endpoint) || endpoint == "gemini-count-tokens" {
		target = geminiTarget(candidate, endpoint)
	} else if isGeminiEndpoint(candidate.Endpoint) {
		geminiEndpoint := "gemini-generate"
		if stream {
			geminiEndpoint = "gemini-stream"
		}
		target = geminiTarget(candidate, geminiEndpoint)
	} else if candidate.Provider == "azure" {
		target = azureTarget(candidate.APIBase, candidate.ModelID, pathEndpoint, candidate.APIVersion)
	} else if azureDocument {
		target, err = azureDocumentOCRTarget(candidate, payload.fields["pages"])
		if err != nil {
			release()
			return nil, err
		}
	} else if candidate.Endpoint == "ocr" && candidate.Provider == "azure_ai" {
		target = strings.TrimRight(candidate.APIBase, "/") + "/providers/mistral/azure/ocr"
	} else if candidate.Endpoint == "ocr" && candidate.Vertex != nil {
		target = vertexOCRTarget(base, candidate)
	} else if candidate.Endpoint == "rerank" && candidate.Vertex != nil {
		target = vertexRerankTarget(candidate)
	} else if candidate.Endpoint == "rerank" && candidate.Bedrock != nil {
		target = strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/rerank") + "/rerank"
	} else if candidate.Endpoint == "rerank" {
		target = rerankTarget(candidate)
	} else if invokeClaude {
		action := "/invoke"
		if stream {
			action = "/invoke-with-response-stream"
		}
		target = strings.TrimRight(candidate.APIBase, "/") + "/model/" + url.PathEscape(candidate.ModelID) + action
	} else if endpoint == "bedrock-count-tokens" {
		target = strings.TrimRight(candidate.APIBase, "/") + "/model/" + url.PathEscape(candidate.ModelID) + "/count-tokens"
	} else if candidate.Endpoint == "bedrock-converse" {
		target = bedrockURL(candidate.APIBase, candidate.ModelID, stream)
	} else if candidate.Endpoint == "bedrock-embeddings" {
		target = strings.TrimRight(candidate.APIBase, "/")
		if strings.HasPrefix(candidate.ModelID, "async_invoke/") {
			target += "/async-invoke"
		} else {
			target += "/model/" + url.PathEscape(candidate.ModelID) + "/invoke"
		}
	} else if candidate.Endpoint == "google-embeddings" {
		target = geminiTarget(candidate, "google-embeddings")
	} else if candidate.Endpoint == "cohere-embeddings" {
		target = cohereEmbeddingTarget(candidate.APIBase)
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		release()
		return nil, err
	}
	upstream.Header.Set("Content-Type", contentType)
	if preview != nil {
		preview.URL = upstream.URL.String()
		preview.Headers = map[string]string{"Content-Type": contentType}
		if json.Valid(encoded) {
			preview.Body = encoded
		}
		release()
		return nil, nil
	}
	apiKey := candidate.APIKey
	if apiKey == "" && candidate.CredentialRef != "" {
		apiKey = os.Getenv(candidate.CredentialRef)
	}
	if speechFormat != "" {
		upstream.Header.Set("X-Microsoft-OutputFormat", speechFormat)
		if apiKey != "" { upstream.Header.Set("Ocp-Apim-Subscription-Key", apiKey) }
		if err := providerKeyHeaders(ctx, upstream.Header, candidate, ""); err != nil {
			release()
			return nil, err
		}
	} else if candidate.Provider == "elevenlabs" {
		upstream.Header.Set("xi-api-key", apiKey)
	} else if candidate.Provider == "deepgram" {
		upstream.Header.Set("Authorization", "Token "+apiKey)
	} else if candidate.Endpoint == "anthropic-messages" {
		upstream.Header.Set("anthropic-version", "2023-06-01")
		for _, header := range []string{"anthropic-version", "anthropic-beta"} {
			if values := request.Header.Values(header); len(values) != 0 {
				upstream.Header[http.CanonicalHeaderKey(header)] = values
			}
		}
		if apiKey != "" {
			upstream.Header.Set("x-api-key", apiKey)
		}
	} else if isGeminiEndpoint(candidate.Endpoint) || candidate.Endpoint == "google-embeddings" || googleImageDeployment(candidate) || candidate.Vertex != nil {
		if err := applyGeminiCredential(upstream, candidate, apiKey); err != nil {
			release()
			return nil, err
		}
	} else if azureDocument {
		upstream.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	} else if candidate.WatsonX != nil && candidate.Endpoint == "rerank" {
		if err := candidate.WatsonX.authorize(upstream, s.config.HTTPClient, apiKey); err != nil {
			release()
			return nil, err
		}
	} else if candidate.Bedrock == nil {
		if err := providerKeyHeaders(ctx, upstream.Header, candidate, apiKey); err != nil {
			release()
			return nil, err
		}
	}
	if vertexSpeechDeployment(candidate) {
		upstream.Header.Set("X-Goog-User-Project", candidate.Vertex.Project)
	}
	for key, value := range candidate.Headers {
		upstream.Header.Set(key, value)
	}
	if session := request.Header.Get("x-opencode-session"); session != "" {
		upstream.Header.Set("x-opencode-session", session)
	}
	if candidate.Bedrock != nil {
		var authErr error
		if candidate.Provider == "aws_polly" {
			authErr = signAWSRequest(ctx, upstream, candidate.Bedrock, encoded, "polly")
		} else {
			authErr = authorizeBedrock(ctx, upstream, candidate, encoded, apiKey)
		}
		if authErr != nil {
			release()
			return nil, authErr
		}
	}
	var response *http.Response
	if azureDocument {
		response, err = s.callAzureDocumentOCR(upstream)
	} else {
		response, err = s.config.HTTPClient.Do(upstream)
	}
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: release}
	if candidate.Endpoint == "bedrock-converse" && stream && response.StatusCode >= 200 && response.StatusCode < 300 {
		if invokeClaude {
			alias, _ := payload.fields["model"].(string)
			response.Body = newBedrockClaudeSSE(response.Body, alias)
		} else {
			response.Body = newBedrockSSEBody(response.Body)
		}
		response.Header.Set("Content-Type", "text/event-stream")
	}
	return response, nil
}

type releaseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (body *releaseBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.release)
	return err
}

func normalizeResponse(endpoint, candidateEndpoint string, body []byte, alias string) []byte {
	if endpoint == "anthropic-count-tokens" {
		return body
	}
	if endpoint == "anthropic-messages" && candidateEndpoint == "bedrock-converse" {
		return rewriteModel(body, alias)
	}
	if candidateEndpoint == "bedrock-converse" {
		var native map[string]any
		if json.Unmarshal(body, &native) != nil || native == nil {
			return body
		}
		chat := bedrockToChat(native, alias)
		if endpoint == "responses" {
			chat = chatToResponses(chat, alias)
		}
		encoded, _ := json.Marshal(chat)
		return encoded
	}
	if candidateEndpoint == "cohere-embeddings" {
		return cohereEmbeddingResponse(body, alias)
	}
	if isGeminiEndpoint(candidateEndpoint) && isTextEndpoint(endpoint) {
		var native map[string]any
		if json.Unmarshal(body, &native) != nil || native == nil {
			return body
		}
		chat := geminiChatResponse(native, alias)
		if endpoint == "responses" {
			chat = chatToResponses(chat, alias)
		}
		encoded, _ := json.Marshal(chat)
		return encoded
	}
	if candidateEndpoint == "anthropic-messages" && isTextEndpoint(endpoint) {
		var native map[string]any
		if json.Unmarshal(body, &native) != nil || native == nil {
			return body
		}
		chat := anthropicToChatResponse(native, alias)
		if endpoint == "responses" {
			chat = chatToResponses(chat, alias)
		}
		encoded, _ := json.Marshal(chat)
		return encoded
	}
	if endpoint != "responses" && endpoint != "chat-completions" && endpoint != "embeddings" && endpoint != "anthropic-messages" && endpoint != "completions" {
		return body
	}
	if endpoint == "chat-completions" && candidateEndpoint == "responses" {
		var response map[string]any
		if json.Unmarshal(body, &response) != nil {
			return body
		}
		encoded, _ := json.Marshal(responsesToChatResponse(response, alias))
		return encoded
	}
	if endpoint != "responses" || candidateEndpoint != "chat-completions" {
		return rewriteModel(body, alias)
	}
	var chat map[string]any
	if json.Unmarshal(body, &chat) != nil {
		return body
	}
	encoded, _ := json.Marshal(chatToResponses(chat, alias))
	return encoded
}

func (s *Server) authorized(request *http.Request) bool {
	if s.config.Token == "" {
		return true
	}
	if s.config.AuthorizeInference {
		return ProxyCredential(request) == s.config.Token
	}
	token, _ := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return token == s.config.Token
}

// ProxyCredential selects the credential from headers in LiteLLM order.
func ProxyCredential(request *http.Request) string {
	if header := request.Header.Get("x-litellm-api-key"); header != "" {
		key, _ := strings.CutPrefix(header, "Bearer ")
		return key
	}
	if header := request.Header.Get("Authorization"); header != "" {
		key, ok := strings.CutPrefix(header, "Bearer ")
		if ok {
			return key
		}
		return ""
	}
	for _, header := range []string{"api-key", "x-api-key", "x-goog-api-key", "Ocp-Apim-Subscription-Key"} {
		if key := request.Header.Get(header); key != "" {
			return key
		}
	}
	if _, endpoint := geminiMethod(request.URL.Path); endpoint != "" {
		return request.URL.Query().Get("key")
	}
	return ""
}

func modelAllowed(scope []string, alias string) bool {
	// A nil scope is unrestricted. An empty intersection denies every model.
	return scope == nil || slices.Contains(scope, "*") || slices.Contains(scope, alias)
}

func candidateAllowed(scope []string, candidate Candidate) bool {
	return candidate.RoutingGroup == "" || modelAllowed(scope, candidate.RoutingGroup)
}

func retryableStatus(status int) bool {
	return status == 408 || status == 409 || status == 429 || status >= 500
}
func retryableResponse(status int, body []byte) bool {
	if retryableStatus(status) {
		return true
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, value := range []string{payload.Error.Code, payload.Error.Type} {
		if value == "model_not_found" || value == "model_unavailable" || value == "invalid_model" {
			return true
		}
	}
	return false
}
func inputBool(input map[string]any, key string) bool { value, _ := input[key].(bool); return value }
func decodeBody(request *http.Request, target any, max int64) error {
	body, err := readBody(request, max)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}
func readBody(request *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, max+1))
	if err != nil || int64(len(body)) > max {
		return nil, errors.New("body_too_large")
	}
	return body, nil
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, _ := json.Marshal(value)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
}
func writeRawResponse(writer http.ResponseWriter, status int, headers http.Header, body []byte) {
	for key, values := range headers {
		if strings.EqualFold(key, "Content-Type") {
			writer.Header()[key] = values
		}
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
func writeResponse(writer http.ResponseWriter, status int, headers http.Header, body []byte) {
	writeRawResponse(writer, status, headers, body)
}
func writeRuntimeMeta(writer http.ResponseWriter, candidate Candidate, attempts int, attemptLog []map[string]any) {
	if recorder, ok := writer.(*callbackResponse); ok {
		recorder.price = candidate.UsagePrice
		recorder.cachePrice = candidate.CacheReadPrice
		recorder.cacheCreationPrice = candidate.CacheCreationPrice
		recorder.event.ModelID = candidate.DeploymentID
		recorder.event.Provider = candidate.Provider
		if target, err := url.Parse(candidate.APIBase); err == nil && target.Host != "" {
			recorder.event.APISource = target.Scheme + "://" + target.Host
		}
	}
	writer.Header().Set("X-Minato-Runtime-Source", candidate.SourceID)
	writer.Header().Set("X-Minato-Runtime-Model", candidate.ModelID)
	writer.Header().Set("X-Minato-Runtime-Attempts", strconv.Itoa(attempts))
	if encoded, err := json.Marshal(attemptLog); err == nil {
		writer.Header().Set("X-Minato-Runtime-Attempt-Log", base64.RawURLEncoding.EncodeToString(encoded))
	}
}
func newUUID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func readSSEFrame(reader *bufio.Reader, max int64) ([]byte, error) {
	frame := make([]byte, 0, 256)
	lineStart := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		if int64(len(frame))+int64(len(fragment)) > max {
			return nil, errors.New("sse_frame_too_large")
		}
		frame = append(frame, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, err
		}
		line := frame[lineStart:]
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return frame, nil
		}
		lineStart = len(frame)
	}
}

func sseFrameFailure(frame []byte) bool {
	if len(frame) == 0 {
		return true
	}
	sawData := false
	for _, line := range bytes.Split(frame, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		sawData = true
		data := strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("data:"))))
		if data == "" || data == "[DONE]" {
			return true
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			return false
		}
		if _, ok := event["error"]; ok {
			return true
		}
		if event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete" {
			return true
		}
		return false
	}
	return !sawData
}
func rewriteModel(body []byte, alias string) []byte {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil || value == nil {
		return body
	}
	value["model"] = alias
	result, _ := json.Marshal(value)
	return result
}
