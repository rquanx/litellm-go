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
	AuthorizeKey       func(string) ([]string, bool)
	RoutingStrategy    string
	LatencyBuffer      float64
	LatencySamples     int
	Callback           func(InferenceEvent)
	LogMessages        bool
	Cooldown           time.Duration
	AllowedFails       int
}

type Server struct {
	config Config
	mu     sync.RWMutex
	routes map[string]Route
	configuredRoutes map[string]Route
	sem    chan struct{}
	trafficMu sync.Mutex
	deploymentTraffic map[string]int
	deploymentLatency map[latencyKey]*latencyWindow
	capacityMinute    int64
	deploymentCapacity map[string]capacityUsage
	deploymentCooldown map[string]cooldownWindow
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
	routes := make(map[string]Route, len(config.Routes))
	for _, route := range config.Routes {
		routes[route.Alias] = route
	}
	return &Server{config: config, routes: make(map[string]Route), configuredRoutes: routes, sem: make(chan struct{}, config.MaxConcurrent)}
}

// ReplaceConfiguredRoutes publishes a complete standalone configuration.
func (s *Server) ReplaceConfiguredRoutes(routes []Route) {
	next := make(map[string]Route, len(routes))
	for _, route := range routes {
		next[route.Alias] = route
	}
	s.mu.Lock()
	s.configuredRoutes = next
	s.mu.Unlock()
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	alias, geminiEndpoint := geminiMethod(request.URL.Path)
	var deployment, deploymentOperation string
	if s.config.AuthorizeInference && geminiEndpoint == "" {
		deployment, deploymentOperation = deploymentEndpoint(request.URL.Path)
		path := request.URL.Path
		if unversioned := proxyAlias(path); unversioned || path == "/openai/v1/responses/compact" {
			normalized := *request.URL
			if unversioned {
				normalized.Path = "/v1" + path
				if normalized.RawPath != "" {
					normalized.RawPath = "/v1" + normalized.RawPath
				}
			} else {
				normalized.Path = strings.TrimPrefix(path, "/openai")
				normalized.RawPath = ""
			}
			copy := request.WithContext(request.Context())
			copy.URL = &normalized
			request = copy
		}
	}
	var modelScope []string
	realtime := isRealtimeSocketPath(request.URL.Path)
	rtHTTP := realtimeHTTPOperation(request.URL.Path)
	if s.config.AuthorizeInference && request.Method == http.MethodPost && rtHTTP == "calls" {
		s.realtimeCall(writer, request)
		return
	}
	if s.config.AuthorizeInference && (realtime || rtHTTP != "" || deploymentOperation != "" || geminiEndpoint != "" ||
		request.URL.Path == "/utils/token_counter" || request.URL.Path == "/model_group/info" ||
		strings.HasPrefix(request.URL.Path, "/v1/")) && !s.authorized(request) {
		allowed := false
		if s.config.AuthorizeKey != nil {
			modelScope, allowed = s.config.AuthorizeKey(inferenceCredential(request))
		}
		if !allowed {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return
		}
	}
	if s.config.AuthorizeInference && request.Method == http.MethodPost && request.URL.Path == "/utils/token_counter" {
		s.countTokens(writer, request, modelScope)
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
		s.responseResource(writer, request, modelScope)
		return
	}
	if s.config.AuthorizeInference && isVectorStorePath(request.URL.Path) {
		s.vectorStoreResource(writer, request, modelScope)
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
		writeError(writer, http.StatusNotFound, "not_found", "Route not found")
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
	if s.config.Callback != nil {
		recorder := &callbackResponse{ResponseWriter: writer, event: newInferenceEvent(endpoint, alias, stream)}
		if key := inferenceCredential(request); key != "" && !s.authorized(request) {
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
	eligible := s.eligibleCandidates(route.Candidates, modelScope, endpoint, stream)
	if len(eligible) == 0 {
		writeError(writer, http.StatusBadRequest, "unsupported_endpoint", "Model does not support this endpoint")
		return
	}
	route.Candidates = eligible
	route.ContextFallback = s.eligibleCandidates(route.ContextFallback, modelScope, endpoint, stream)
	route.PolicyFallback = s.eligibleCandidates(route.PolicyFallback, modelScope, endpoint, stream)
	if s.config.AuthorizeInference && endpoint == "responses" {
		previous, _ := input["previous_response_id"].(string)
		provider, deployment, original, wrapped := unwrapResponseID(previous)
		if wrapped {
			input["previous_response_id"] = original
		} else {
			provider, deployment = "", ""
		}
		if err := rewriteResponseVectorStores(input, &deployment); err != nil {
			writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if deployment != "" && !route.pinResponseDeployment(provider, deployment) {
			writeError(writer, http.StatusBadRequest, "response_not_found", "Response resources do not belong to this route")
			return
		}
	}
	if stream {
		if !endpoints[endpoint].streaming {
			writeError(writer, http.StatusBadRequest, "streaming_unsupported", "This endpoint does not support event streams")
			return
		}
		s.inferStream(writer, request, endpoint, route, payload)
		return
	}
	attempts := make([]map[string]any, 0, len(route.Candidates))
	plan := attemptPlan{retries: s.config.NumRetries, remaining: s.config.NumRetries}
	for {
		candidate := route.Candidates[plan.index]
		started := time.Now()
		reservation, admitted := s.admitDeployment(candidate, payload)
		var status int
		var responseBody []byte
		var responseHeaders http.Header
		var retryable bool
		var callErr error
		if admitted {
			status, responseBody, responseHeaders, retryable, callErr = s.callJSON(request.Context(), request, endpoint, candidate, payload)
			s.recordCooldown(candidate, status, retryable, responseHeaders)
			if reservation != nil && status >= 200 && status < 300 {
				s.settleCapacity(reservation, usageFromJSON(responseBody))
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
				if candidate.Endpoint == "anthropic-messages" && endpoint != "anthropic-count-tokens" {
					usage, _ := payload["usage"].(map[string]any)
					attempt["usage"] = anthropicUsage(usage)
				}
				if candidate.Endpoint == "bedrock-converse" {
					attempt["usage"] = bedrockUsage(payload["usage"])
				}
			}
		}
		attempts = append(attempts, attempt)
		if callErr == nil && status >= 200 && status < 300 {
			s.recordLatency(candidate, false, time.Since(started))
			writeRuntimeMeta(writer, candidate, len(attempts), attempts)
			body := normalizeResponse(endpoint, candidate.Endpoint, responseBody, route.Alias)
			if s.config.AuthorizeInference && endpoint == "responses" && candidate.Endpoint == "responses" && candidate.DeploymentID != "" {
				body = wrapResponseBody(body, candidate)
			}
			writeResponse(writer, status, responseHeaders, body)
			return
		}
		if !plan.next(request.Context(), status, responseHeaders, retryable, responseBody, &route) {
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

func (s *Server) inferStream(writer http.ResponseWriter, request *http.Request, endpoint string, route Route, payload requestPayload) {
	attempts := make([]map[string]any, 0, len(route.Candidates))
	plan := attemptPlan{retries: s.config.NumRetries, remaining: s.config.NumRetries}
	for {
		candidate := route.Candidates[plan.index]
		started := time.Now()
		reservation, admitted := s.admitDeployment(candidate, payload)
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
			s.recordCooldown(candidate, status, retryable, headers)
		}
		attempt := map[string]any{"id": newUUID(), "sourceId": candidate.SourceID, "modelId": candidate.ModelID,
			"status": status, "durationMs": time.Since(started).Milliseconds(), "retryable": retryable}
		if err != nil {
			attempt["status"] = http.StatusBadGateway
		}
		attempts = append(attempts, attempt)
		if err == nil && !streamFailure && status >= 200 && status < 300 {
			s.recordLatency(candidate, true, time.Since(started))
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
				convert = func(frame []byte) []byte { return wrapResponseEvent(frame, candidate) }
			}
			if reservation != nil {
				baseConvert := convert
				convert = func(frame []byte) []byte {
					s.settleCapacity(reservation, usageFromJSON(streamErrorBody(frame)))
					return baseConvert(frame)
				}
			}
			if len(firstFrame) > 0 && !writeStreamFrame(writer, flush, convert(firstFrame)) {
				return
			}
			for {
				frame, readErr := readSSEFrame(reader, s.config.MaxBodyBytes)
				if len(frame) > 0 && !writeStreamFrame(writer, flush, convert(frame)) {
					return
				}
				if readErr != nil {
					if readErr == io.EOF && isGeminiEndpoint(candidate.Endpoint) && isTextEndpoint(endpoint) {
						writeStreamFrame(writer, flush, convert(nil))
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
		if !plan.next(request.Context(), status, headers, retryable, errorBody, &route) {
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

func (s *Server) eligibleCandidates(candidates []Candidate, scope []string, endpoint string, stream bool) []Candidate {
	if candidates == nil {
		return nil
	}
	eligible := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidateAllowed(scope, candidate) && servesEndpoint(endpoint, candidate.Endpoint) {
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
	s.orderDeployments(eligible, stream)
	return eligible
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
	if candidate.Endpoint == "bedrock-embeddings" {
		return s.bedrockEmbeddings(ctx, request, candidate, payload)
	}
	if candidate.Endpoint == "google-embeddings" {
		return s.googleEmbeddings(ctx, request, candidate, payload)
	}
	return s.callProviderJSON(ctx, request, endpoint, candidate, payload)
}

func (s *Server) callProviderJSON(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload) (int, []byte, http.Header, bool, error) {
	response, err := s.send(ctx, request, endpoint, candidate, payload, false)
	if err != nil {
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
	body, err := json.Marshal(payload)
	return body, "application/json", err
}

func (s *Server) send(ctx context.Context, request *http.Request, endpoint string, candidate Candidate, payload requestPayload, stream bool) (*http.Response, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	finished := s.trackDeployment(candidate)
	release := func() {
		finished()
		<-s.sem
	}
	encoded, contentType, err := encodePayload(payload, endpoint, candidate, stream)
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
	if isGeminiEndpoint(endpoint) || endpoint == "gemini-count-tokens" {
		target = geminiTarget(candidate, endpoint)
	} else if isGeminiEndpoint(candidate.Endpoint) {
		geminiEndpoint := "gemini-generate"
		if stream {
			geminiEndpoint = "gemini-stream"
		}
		target = geminiTarget(candidate, geminiEndpoint)
	} else if candidate.Provider == "azure" {
		target = azureTarget(candidate.APIBase, candidate.ModelID, pathEndpoint, candidate.APIVersion)
	} else if candidate.Endpoint == "rerank" {
		target = rerankTarget(candidate.APIBase, candidate.Provider)
	} else if candidate.Endpoint == "bedrock-converse" {
		target = bedrockURL(candidate.APIBase, candidate.ModelID, stream)
	} else if candidate.Endpoint == "bedrock-embeddings" {
		target = strings.TrimRight(candidate.APIBase, "/") + "/model/" + url.PathEscape(candidate.ModelID) + "/invoke"
	} else if candidate.Endpoint == "google-embeddings" {
		target = geminiTarget(candidate, "google-embeddings")
	}
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		release()
		return nil, err
	}
	upstream.Header.Set("Content-Type", contentType)
	apiKey := candidate.APIKey
	if apiKey == "" && candidate.CredentialRef != "" {
		apiKey = os.Getenv(candidate.CredentialRef)
	}
	if candidate.Endpoint == "anthropic-messages" {
		upstream.Header.Set("anthropic-version", "2023-06-01")
		for _, header := range []string{"anthropic-version", "anthropic-beta"} {
			if values := request.Header.Values(header); len(values) != 0 {
				upstream.Header[http.CanonicalHeaderKey(header)] = values
			}
		}
		if apiKey != "" {
			upstream.Header.Set("x-api-key", apiKey)
		}
	} else if isGeminiEndpoint(candidate.Endpoint) || candidate.Endpoint == "google-embeddings" {
		if err := applyGeminiCredential(upstream, candidate, apiKey); err != nil {
			release()
			return nil, err
		}
	} else if candidate.Bedrock == nil {
		providerKeyHeaders(upstream.Header, candidate, apiKey)
	}
	for key, value := range candidate.Headers {
		upstream.Header.Set(key, value)
	}
	if session := request.Header.Get("x-opencode-session"); session != "" {
		upstream.Header.Set("x-opencode-session", session)
	}
	if candidate.Bedrock != nil {
		if err := authorizeBedrock(ctx, upstream, candidate, encoded, apiKey); err != nil {
			release()
			return nil, err
		}
	}
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: release}
	if candidate.Endpoint == "bedrock-converse" && stream && response.StatusCode >= 200 && response.StatusCode < 300 {
		response.Body = newBedrockSSEBody(response.Body)
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
	if endpoint == "anthropic-count-tokens" || (endpoint != "responses" && endpoint != "chat-completions" && endpoint != "embeddings" && endpoint != "anthropic-messages" && endpoint != "completions") {
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
	return s.config.Token == "" || inferenceCredential(request) == s.config.Token
}

func inferenceCredential(request *http.Request) string {
	if strings.HasPrefix(request.URL.Path, "/openai/deployments/") {
		if key := request.Header.Get("api-key"); key != "" {
			return key
		}
	}
	if _, endpoint := geminiMethod(request.URL.Path); endpoint != "" {
		if key := request.Header.Get("x-goog-api-key"); key != "" {
			return key
		}
	}
	if request.URL.Path == "/v1/messages" || request.URL.Path == "/v1/messages/count_tokens" {
		if key := request.Header.Get("x-api-key"); key != "" {
			return key
		}
	}
	if token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer "); ok {
		return token
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
		recorder.event.ModelID = candidate.DeploymentID
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
