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
	candidates := s.eligibleCandidates(route.Candidates, scope, "responses", true)
	if !hasNativeResponsesSocket(candidates) {
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "No eligible native Responses WebSocket deployment")
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

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		socketClientToProvider(ctx, client, upstream, alias, candidate)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		socketProviderToClient(ctx, upstream, client, alias, candidate)
	}()
	<-done
	cancel()
	<-done
}

func nativeResponsesSocket(candidate Candidate) bool {
	return candidate.Endpoint == "responses" && (candidate.Provider == "openai" || candidate.Provider == "azure")
}

func hasNativeResponsesSocket(candidates []Candidate) bool {
	for _, candidate := range candidates {
		if nativeResponsesSocket(candidate) {
			return true
		}
	}
	return false
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
	headers := make(http.Header)
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if key != "" {
		if candidate.Provider == "azure" {
			headers.Set("api-key", key)
		} else {
			headers.Set("Authorization", "Bearer "+key)
		}
	}
	conn, _, err := websocket.Dial(ctx, address.String(), &websocket.DialOptions{
		HTTPClient: s.config.HTTPClient, HTTPHeader: headers,
	})
	return conn, err
}

func socketClientToProvider(ctx context.Context, client, upstream *websocket.Conn, alias string, candidate Candidate) {
	for {
		kind, data, err := client.Read(ctx)
		if err != nil {
			return
		}
		event, err := prepareSocketEvent(kind, data, alias, candidate)
		if err != nil {
			payload, _ := json.Marshal(map[string]any{"type": "error", "status": 400,
				"error": map[string]string{"code": "invalid_request", "message": err.Error()}})
			if client.Write(ctx, websocket.MessageText, payload) != nil {
				return
			}
			continue
		}
		if upstream.Write(ctx, websocket.MessageText, event) != nil {
			return
		}
	}
}

func prepareSocketEvent(kind websocket.MessageType, data []byte, alias string, candidate Candidate) ([]byte, error) {
	var event map[string]any
	if kind != websocket.MessageText || json.Unmarshal(data, &event) != nil || event["type"] != "response.create" {
		return nil, errors.New("expected a response.create JSON event")
	}
	if model, supplied := event["model"]; supplied && model != alias {
		return nil, errors.New("event model differs from the WebSocket session model")
	}
	event["model"] = candidate.ModelID
	deployment := candidate.DeploymentID
	if previous, _ := event["previous_response_id"].(string); previous != "" {
		if provider, id, original, wrapped := unwrapResponseID(previous); wrapped {
			if provider != candidate.Provider || id != deployment {
				return nil, errors.New("previous response belongs to a different deployment")
			}
			event["previous_response_id"] = original
		}
	}
	if err := rewriteResponseVectorStores(event, &deployment); err != nil {
		return nil, err
	}
	return json.Marshal(event)
}

func socketProviderToClient(ctx context.Context, upstream, client *websocket.Conn, alias string, candidate Candidate) {
	for {
		kind, data, err := upstream.Read(ctx)
		if err != nil {
			return
		}
		if kind == websocket.MessageText {
			var event map[string]json.RawMessage
			if json.Unmarshal(data, &event) == nil {
				if raw := event["response"]; len(raw) != 0 && string(raw) != "null" {
					response := rewriteModel(raw, alias)
					if candidate.DeploymentID != "" {
						response = wrapResponseBody(response, candidate)
					}
					event["response"] = response
					data, _ = json.Marshal(event)
				}
			}
		}
		if client.Write(ctx, kind, data) != nil {
			return
		}
	}
}
