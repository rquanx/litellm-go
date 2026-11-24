package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) responsesSocket(w http.ResponseWriter, request *http.Request, scope []string) {
	alias := request.URL.Query().Get("model")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "model_required", "A model is required for the WebSocket session")
		return
	}
	if !modelAllowed(scope, alias) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	s.mu.RLock()
	route, exists := s.configuredRoutes[alias]
	s.mu.RUnlock()
	if !exists {
		writeError(w, http.StatusNotFound, "model_not_found", "Unknown model")
		return
	}
	candidates := s.eligibleCandidates(request.Context(), route.Candidates, scope, "responses", true)
	if len(candidates) == 0 {
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "No eligible Responses deployment")
		return
	}
	if !nativeResponsesSocket(candidates[0]) {
		s.managedResponsesSocket(w, request, alias)
		return
	}
	budget, allowed := s.readKeyBudget(w, request, ProxyCredential(request), false)
	if !allowed {
		return
	}
	session, allowed := s.admitKeyLimits(w, request, budget, -1)
	if !allowed {
		return
	}
	defer session.finish(nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.sem }()

	var upstream *websocket.Conn
	var candidate Candidate
	for _, deployment := range candidates {
		if !nativeResponsesSocket(deployment) {
			continue
		}
		conn, err := s.dialResponsesSocket(ctx, deployment)
		if err == nil {
			upstream, candidate = conn, deployment
			break
		}
	}
	if upstream == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not open the provider WebSocket")
		return
	}
	defer upstream.CloseNow()
	client, err := acceptInferenceSocket(w, request)
	if err != nil {
		return
	}
	defer client.CloseNow()
	upstream.SetReadLimit(s.config.MaxBodyBytes)
	client.SetReadLimit(s.config.MaxBodyBytes)

	var tokens responseSocketTokens
	defer tokens.close()
	runSocketRelay(ctx, func(ctx context.Context) {
		s.socketClientToProvider(ctx, request, client, upstream, alias, candidate, &tokens)
	}, func(ctx context.Context) {
		s.socketProviderToClient(ctx, request, upstream, client, alias, candidate,
			s.responseContainerEncoder(request.WithContext(ctx), candidate), budget, &tokens)
	})
}

func acceptInferenceSocket(w http.ResponseWriter, request *http.Request) (*websocket.Conn, error) {
	for _, header := range request.Header.Values("Sec-WebSocket-Protocol") {
		for _, value := range strings.Split(header, ",") {
			protocol := strings.TrimSpace(value)
			if protocol != "" && !strings.HasPrefix(protocol, "openai-insecure-api-key.") {
				return websocket.Accept(w, request, &websocket.AcceptOptions{Subprotocols: []string{protocol}})
			}
		}
	}
	return websocket.Accept(w, request, nil)
}

func nativeResponsesSocket(candidate Candidate) bool {
	return candidate.Endpoint == "responses" && (candidate.Provider == "openai" || candidate.Provider == "azure")
}

func (s *Server) dialResponsesSocket(ctx context.Context, candidate Candidate) (*websocket.Conn, error) {
	base := strings.TrimRight(candidate.APIBase, "/")
	target := strings.TrimSuffix(base, "/v1") + "/v1/responses"
	if candidate.Provider == "azure" {
		target = azureTarget(base, candidate.ModelID, "responses", candidate.APIVersion)
	}
	address, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	query := address.Query()
	query.Set("model", candidate.ModelID)
	address.RawQuery = query.Encode()
	return s.dialProviderSocket(ctx, candidate, address.String(), make(http.Header))
}

func (s *Server) dialProviderSocket(ctx context.Context, candidate Candidate, address string, headers http.Header) (*websocket.Conn, error) {
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if err := providerKeyHeaders(ctx, headers, candidate, key); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{
		HTTPClient: s.config.HTTPClient, HTTPHeader: headers,
	})
	return conn, err
}

func runSocketRelay(ctx context.Context, clientToProvider, providerToClient func(context.Context)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		clientToProvider(ctx)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		providerToClient(ctx)
	}()
	<-done
	cancel()
	<-done
}

func (s *Server) socketStillAllowed(request *http.Request, alias string, candidate Candidate) bool {
	if s.authorized(request) {
		return true
	}
	if s.config.AuthorizeKey == nil {
		return false
	}
	scope, allowed := s.config.AuthorizeKey(ProxyCredential(request))
	return allowed && modelAllowed(scope, alias) && candidateAllowed(scope, candidate)
}

func (s *Server) socketClientToProvider(ctx context.Context, request *http.Request, client, upstream *websocket.Conn, alias string, candidate Candidate, tokens *responseSocketTokens) {
	for {
		kind, data, err := client.Read(ctx)
		if err != nil {
			return
		}
		if !s.socketStillAllowed(request, alias, candidate) {
			_ = writeSocketError(ctx, client, http.StatusForbidden, "The key cannot use this WebSocket session")
			return
		}
		event, ids, err := s.prepareSocketEvent(request.WithContext(ctx), kind, data, alias, candidate)
		if err != nil {
			if writeSocketError(ctx, client, http.StatusBadRequest, err.Error()) != nil {
				return
			}
			continue
		}
		if status, message := s.checkResponseContainerOwners(request.WithContext(ctx), candidate.DeploymentID, ids); status != 0 {
			if writeSocketError(ctx, client, status, message) != nil {
				return
			}
			continue
		}
		budget, allowed := s.socketTurnBudget(ctx, request, client, alias)
		if !allowed {
			return
		}
		estimated := 0
		if budget.hasTokenLimit() {
			var fields map[string]any
			_ = json.Unmarshal(event, &fields)
			estimated = max(1, estimatedInputTokens(fields))
		}
		if estimated == 0 && s.config.Callback == nil {
			if upstream.Write(ctx, websocket.MessageText, event) != nil {
				return
			}
			continue
		}
		admitted, err := tokens.reserve(ctx, s, budget, estimated)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read key token limit")
			return
		}
		if !admitted {
			if writeSocketError(ctx, client, http.StatusTooManyRequests, "Key token limit exceeded") != nil {
				return
			}
			continue
		}
		if upstream.Write(ctx, websocket.MessageText, event) != nil {
			return
		}
	}
}

func (s *Server) prepareSocketEvent(request *http.Request, kind websocket.MessageType, data []byte, alias string, candidate Candidate) ([]byte, []string, error) {
	event, err := parseSocketEvent(kind, data, alias)
	if err != nil {
		return nil, nil, err
	}
	event["type"] = "response.create"
	event["model"] = candidate.ModelID
	deployment := candidate.DeploymentID
	if previous, _ := event["previous_response_id"].(string); previous != "" {
		if provider, id, original, wrapped := unwrapResponseID(previous); wrapped {
			if provider != candidate.Provider || id != deployment {
				return nil, nil, errors.New("previous response belongs to a different deployment")
			}
			event["previous_response_id"] = original
		}
	}
	// The session has already authorized the fixed alias and deployment.
	if err := s.rewriteResponseVectorStores(request, event, alias, nil, &deployment); err != nil {
		return nil, nil, err
	}
	ids, err := rewriteResponseContainers(event, &deployment)
	if err != nil {
		return nil, nil, err
	}
	if deployment != candidate.DeploymentID {
		return nil, nil, errors.New("container belongs to a different deployment")
	}
	encoded, err := json.Marshal(event)
	return encoded, ids, err
}

func parseSocketEvent(kind websocket.MessageType, data []byte, alias string) (map[string]any, error) {
	var event map[string]any
	if kind != websocket.MessageText || json.Unmarshal(data, &event) != nil || event["type"] != "response.create" {
		return nil, errors.New("expected a response.create JSON event")
	}
	if nested, ok := event["response"].(map[string]any); ok && nested != nil {
		event = nested
	}
	if model, supplied := event["model"]; supplied && model != alias {
		return nil, errors.New("event model differs from the WebSocket session model")
	}
	delete(event, "type")
	return event, nil
}

func (s *Server) socketProviderToClient(ctx context.Context, request *http.Request, upstream, client *websocket.Conn, alias string, candidate Candidate, encodeContainer func(string) string, budget KeyBudget, tokens *responseSocketTokens) {
	for {
		kind, data, err := upstream.Read(ctx)
		if err != nil {
			return
		}
		if kind == websocket.MessageText {
			var event map[string]json.RawMessage
			if json.Unmarshal(data, &event) == nil {
				var eventType string
				_ = json.Unmarshal(event["type"], &eventType)
				turn := budget
				switch eventType {
				case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "error":
					if admitted, ok := tokens.settle(usageFromJSON(data)); ok {
						turn = admitted
					}
				}
				if eventType == "response.completed" {
					s.recordSocketUsage(request, candidate, alias, "responses", data, turn)
				}
				changed := false
				if raw := event["response"]; len(raw) != 0 && string(raw) != "null" {
					response := rewriteModel(raw, alias)
					if candidate.DeploymentID != "" {
						response = wrapResponseBody(response, candidate, encodeContainer)
					}
					event["response"] = response
					changed = true
				}
				var item map[string]any
				if raw := event["item"]; raw != nil && json.Unmarshal(raw, &item) == nil && encodeOutputContainerIDs(item, encodeContainer) {
					event["item"], _ = json.Marshal(item)
					changed = true
				}
				if changed {
					data, _ = json.Marshal(event)
				}
			}
		}
		if client.Write(ctx, kind, data) != nil {
			return
		}
	}
}

func (s *Server) socketTurnBudget(ctx context.Context, r *http.Request, client *websocket.Conn, model string) (KeyBudget, bool) {
	if s.config.CheckGlobalBudget != nil {
		message, err := s.config.CheckGlobalBudget(ctx)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read proxy budget")
			return KeyBudget{}, false
		}
		if message != "" {
			_ = writeSocketError(ctx, client, http.StatusTooManyRequests, message)
			return KeyBudget{}, false
		}
	}
	if s.config.CheckKeyBudget == nil || s.authorized(r) {
		return KeyBudget{Tracked: s.config.CheckGlobalBudget != nil}, true
	}
	budget, err := s.config.CheckKeyBudget(ctx, ProxyCredential(r))
	if err != nil {
		_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read key budget")
		return KeyBudget{}, false
	}
	if budget.ErrorMessage != "" {
		_ = writeSocketError(ctx, client, http.StatusTooManyRequests, budget.ErrorMessage)
		return KeyBudget{}, false
	}
	budget.Tracked = budget.Tracked || s.config.CheckGlobalBudget != nil
	if budget.ModelPolicy != nil {
		policy, err := budget.ModelPolicy(ctx, model)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read key model budget")
			return KeyBudget{}, false
		}
		if policy.BudgetError != "" {
			_ = writeSocketError(ctx, client, http.StatusTooManyRequests, policy.BudgetError)
			return KeyBudget{}, false
		}
		budget.ModelSpendDuration = policy.SpendDuration
		budget.Tracked = budget.Tracked || policy.SpendDuration != ""
	}
	if budget.ModelRates != nil {
		budget.OwnerLimits = append(budget.OwnerLimits, budget.ModelRates(model, s.deploymentModelRateDefaults(model))...)
	}
	if budget.RPMLimit != nil || len(budget.OwnerLimits) != 0 {
		retryAfter, err := s.reserveRequestRates(ctx, budget)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read key rate limit")
			return KeyBudget{}, false
		}
		if retryAfter > 0 {
			_ = writeSocketError(ctx, client, http.StatusTooManyRequests, "Key request limit exceeded")
			return KeyBudget{}, false
		}
	}
	return budget, true
}

func (s *Server) recordSocketUsage(r *http.Request, candidate Candidate, alias, endpoint string, data []byte, budget KeyBudget) {
	if s.config.Callback == nil {
		return
	}
	var terminal struct {
		Response struct {
			ID        string  `json:"id"`
			CreatedAt float64 `json:"created_at"`
			Status    string  `json:"status"`
			Usage     struct {
				InputDetails realtimeTokenDetails `json:"input_token_details"`
				OutputDetails realtimeTokenDetails `json:"output_token_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &terminal) != nil || terminal.Response.ID == "" {
		return
	}
	event := newInferenceEvent(endpoint, alias, true)
	// A reconnect may replay the same terminal event.
	event.ID = fmt.Sprintf("socket-%x", sha256.Sum256([]byte(candidate.DeploymentID+"\x00"+candidate.APIBase+"\x00"+terminal.Response.ID)))
	if created := terminal.Response.CreatedAt; created > 0 && created <= float64(time.Now().Unix()) {
		event.StartTime = created
	}
	budget.apply(&event)
	if key := ProxyCredential(r); key != "" && !s.authorized(r) {
		event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	}
	status := http.StatusOK
	if terminal.Response.Status == "failed" {
		status = http.StatusBadGateway
	}
	recorder := callbackResponse{
		ResponseWriter: &socketStreamWriter{headers: make(http.Header)},
		event: event, status: status, capture: s.config.LogMessages,
	}
	writeRuntimeMeta(&recorder, candidate, 1, nil)
	recorder.recordJSON(data)
	recorder.finish()
	if endpoint == "realtime" {
		recorder.event.Spend = realtimeResponseCost(recorder.event, terminal.Response.Usage.InputDetails, terminal.Response.Usage.OutputDetails, candidate)
	}
	s.config.Callback(recorder.event)
}
