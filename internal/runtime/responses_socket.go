package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

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
	client, err := websocket.Accept(w, request, nil)
	if err != nil {
		return
	}
	defer client.CloseNow()
	upstream.SetReadLimit(s.config.MaxBodyBytes)
	client.SetReadLimit(s.config.MaxBodyBytes)

	runSocketRelay(ctx, func(ctx context.Context) {
		s.socketClientToProvider(ctx, request, client, upstream, alias, candidate)
	}, func(ctx context.Context) {
		socketProviderToClient(ctx, upstream, client, alias, candidate, s.responseContainerEncoder(request.WithContext(ctx), candidate))
	})
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
	providerKeyHeaders(headers, candidate, key)
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

func (s *Server) socketClientToProvider(ctx context.Context, request *http.Request, client, upstream *websocket.Conn, alias string, candidate Candidate) {
	for {
		kind, data, err := client.Read(ctx)
		if err != nil {
			return
		}
		if !s.socketStillAllowed(request, alias, candidate) {
			_ = writeSocketError(ctx, client, http.StatusForbidden, "The key cannot use this WebSocket session")
			return
		}
		event, ids, err := prepareSocketEvent(kind, data, alias, candidate)
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
		if upstream.Write(ctx, websocket.MessageText, event) != nil {
			return
		}
	}
}

func prepareSocketEvent(kind websocket.MessageType, data []byte, alias string, candidate Candidate) ([]byte, []string, error) {
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
	if err := rewriteResponseVectorStores(event, &deployment); err != nil {
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

func socketProviderToClient(ctx context.Context, upstream, client *websocket.Conn, alias string, candidate Candidate, encodeContainer func(string) string) {
	for {
		kind, data, err := upstream.Read(ctx)
		if err != nil {
			return
		}
		if kind == websocket.MessageText {
			var event map[string]json.RawMessage
			if json.Unmarshal(data, &event) == nil {
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
