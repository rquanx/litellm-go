package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

func isRealtimeSocketPath(path string) bool {
	return path == "/realtime" || path == "/v1/realtime" || path == "/openai/v1/realtime"
}

func (s *Server) realtimeSocket(w http.ResponseWriter, request *http.Request, scope []string) {
	query := request.URL.Query()
	alias := query.Get("model")
	if alias == "" || query.Get("guardrails") != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "A model is required and session guardrail overrides are unsupported")
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
	ctx := request.Context()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.sem }()
	var upstream *websocket.Conn
	var candidate Candidate
	for _, deployment := range route.Candidates {
		if !candidateAllowed(scope, deployment) || (deployment.Provider != "openai" && deployment.Provider != "azure") {
			continue
		}
		address := realtimeAddress(deployment, query.Get("intent"))
		headers := make(http.Header)
		if request.Header.Get("OpenAI-Beta") == "realtime=v1" {
			headers.Set("OpenAI-Beta", "realtime=v1")
		}
		conn, err := s.dialProviderSocket(ctx, deployment, address, headers)
		if err == nil {
			upstream, candidate = conn, deployment
			break
		}
	}
	if upstream == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not open the provider Realtime session")
		return
	}
	defer upstream.CloseNow()
	client, err := websocket.Accept(w, request, &websocket.AcceptOptions{Subprotocols: []string{"realtime"}})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(s.config.MaxBodyBytes)
	upstream.SetReadLimit(s.config.MaxBodyBytes)
	runSocketRelay(ctx, func(ctx context.Context) {
		for {
			kind, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			if !s.socketStillAllowed(request, alias, candidate) {
				_ = writeSocketError(ctx, client, http.StatusForbidden, "The key cannot use this Realtime session")
				return
			}
			data, err = realtimeEvent(kind, data, alias, candidate.ModelID)
			if err != nil {
				if writeSocketError(ctx, client, http.StatusBadRequest, err.Error()) != nil {
					return
				}
				continue
			}
			if upstream.Write(ctx, kind, data) != nil {
				return
			}
		}
	}, func(ctx context.Context) {
		for {
			kind, data, err := upstream.Read(ctx)
			if err != nil || client.Write(ctx, kind, data) != nil {
				return
			}
		}
	})
}

func realtimeAddress(candidate Candidate, intent string) string {
	base := strings.TrimRight(candidate.APIBase, "/")
	query := url.Values{"model": {candidate.ModelID}}
	if intent != "" {
		query.Set("intent", intent)
	}
	if candidate.Provider != "azure" {
		return strings.TrimSuffix(base, "/v1") + "/v1/realtime?" + query.Encode()
	}
	if strings.HasSuffix(base, "/openai/v1") {
		return base + "/realtime?" + query.Encode()
	}
	query.Del("model")
	query.Set("deployment", candidate.ModelID)
	query.Set("api-version", candidate.APIVersion)
	return strings.TrimSuffix(base, "/openai") + "/openai/realtime?" + query.Encode()
}

func realtimeEvent(kind websocket.MessageType, data []byte, alias, model string) ([]byte, error) {
	var event map[string]any
	if kind != websocket.MessageText || json.Unmarshal(data, &event) != nil || event == nil {
		return nil, errors.New("expected a Realtime JSON event")
	}
	changed := false
	for _, container := range []map[string]any{event, objectField(event, "session"), objectField(event, "response")} {
		if value, exists := container["model"]; exists {
			if value != alias {
				return nil, errors.New("event model differs from the Realtime session model")
			}
			container["model"] = model
			changed = true
		}
	}
	if changed {
		return json.Marshal(event)
	}
	return data, nil
}

func objectField(fields map[string]any, key string) map[string]any {
	value, _ := fields[key].(map[string]any)
	return value
}
