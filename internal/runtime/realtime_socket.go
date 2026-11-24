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
	budget, allowed := s.readKeyBudget(w, request, ProxyCredential(request), false)
	if !allowed {
		return
	}
	s.mu.RLock()
	route, exists := s.configuredRoutes[alias]
	s.mu.RUnlock()
	if !exists {
		writeError(w, http.StatusNotFound, "model_not_found", "Unknown model")
		return
	}
	if budget.ModelPolicy != nil {
		policy, allowed := s.checkKeyModelPolicy(w, request, budget.ModelPolicy, alias)
		if !allowed {
			return
		}
		budget.ModelSpendDuration = policy.SpendDuration
		budget.Tracked = budget.Tracked || policy.SpendDuration != ""
	}
	if budget.ModelRates != nil {
		budget.OwnerLimits = append(budget.OwnerLimits, budget.ModelRates(alias, s.deploymentModelRateDefaults(alias))...)
	}
	session, allowed := s.admitKeyLimits(w, request, budget, -1)
	if !allowed {
		return
	}
	defer session.finish(nil)
	ctx := request.Context()
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.sem }()
	var upstream *websocket.Conn
	var bedrock *bedrockRealtimeStream
	var candidate Candidate
	for _, deployment := range route.Candidates {
		if !candidateAllowed(scope, deployment) {
			continue
		}
		if bedrockRealtimeDeployment(deployment) {
			stream, err := s.openBedrockRealtime(ctx, deployment)
			if err == nil {
				bedrock, candidate = stream, deployment
				break
			}
			continue
		}
		if !geminiLiveProvider(deployment) && !supportsNativeRealtimeSocket(deployment.Provider) {
			continue
		}
		conn, err := s.dialRealtimeSocket(ctx, deployment, query.Get("intent"), request.Header.Get("OpenAI-Beta"))
		if err == nil {
			upstream, candidate = conn, deployment
			break
		}
	}
	if upstream == nil && bedrock == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not open the provider Realtime session")
		return
	}
	if bedrock != nil {
		defer bedrock.close()
	} else {
		defer upstream.CloseNow()
		upstream.SetReadLimit(s.config.MaxBodyBytes)
	}
	client, err := acceptInferenceSocket(w, request)
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(s.config.MaxBodyBytes)
	checkTurns := s.config.CheckGlobalBudget != nil || (s.config.CheckKeyBudget != nil && !s.authorized(request))
	limitAutomaticTurns := budget.Tracked || budget.RPMLimit != nil || budget.hasTokenLimit()
	for _, owner := range budget.OwnerLimits {
		limitAutomaticTurns = limitAutomaticTurns || owner.RPM != nil
	}
	var vad *realtimeVAD
	if checkTurns && limitAutomaticTurns && query.Get("intent") != "transcription" &&
		(candidate.Provider == "openai" || candidate.Provider == "azure") {
		vad = newRealtimeVAD()
	}
	admitTurn := func(ctx context.Context) (allowed, keepOpen bool) {
		if !s.socketStillAllowed(request, alias, candidate) {
			_ = writeSocketError(ctx, client, http.StatusForbidden, "The key cannot use this Realtime session")
			return false, false
		}
		turn, allowed := s.socketTurnBudget(ctx, request, client, alias)
		if !allowed {
			return false, false
		}
		admitted, err := s.checkRealtimeTokens(ctx, turn)
		if err != nil {
			_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not read key token limit")
			return false, false
		}
		if !admitted {
			return false, writeSocketError(ctx, client, http.StatusTooManyRequests, "Key token limit exceeded") == nil
		}
		return true, true
	}
	if bedrock != nil {
		s.relayBedrockRealtime(ctx, request, client, bedrock, candidate, alias, budget, checkTurns, admitTurn)
		return
	}
	if geminiLiveProvider(candidate) {
		s.relayGeminiLive(ctx, request, client, upstream, candidate, alias, budget, checkTurns, admitTurn)
		return
	}
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
			var event struct{ Type string }
			if checkTurns {
				_ = json.Unmarshal(data, &event)
			}
			if event.Type == "response.create" {
				allowed, keepOpen := admitTurn(ctx)
				if !keepOpen {
					return
				}
				if !allowed {
					continue
				}
			}
			if vad != nil {
				switch event.Type {
				case "session.update", "input_audio_buffer.append", "input_audio_buffer.commit":
					if !waitRealtimeSettings(ctx, vad.ready) {
						_ = writeSocketError(ctx, client, http.StatusBadGateway, "Provider Realtime audio settings were not confirmed")
						return
					}
				}
				if event.Type == "session.update" {
					var fields map[string]any
					_ = json.Unmarshal(data, &fields)
					update := vad.prepareUpdate(fields)
					data, _ = json.Marshal(fields)
					if upstream.Write(ctx, kind, data) != nil {
						return
					}
					if !waitRealtimeSettings(ctx, update.done) {
						_ = writeSocketError(ctx, client, http.StatusBadGateway, "Provider Realtime audio settings were not confirmed")
						return
					}
					continue
				}
			}
			if upstream.Write(ctx, kind, data) != nil {
				return
			}
		}
	}, func(ctx context.Context) {
		var accounting realtimeTokenUsage
		providerSessionSeen := false
		for {
			kind, data, err := upstream.Read(ctx)
			if err != nil {
				return
			}
			var event struct {
				Type string
				ItemID string `json:"item_id"`
				Error struct{ EventID string `json:"event_id"` }
			}
			if kind == websocket.MessageText && (vad != nil || s.config.Callback != nil || budget.hasTokenLimit()) {
				_ = json.Unmarshal(data, &event)
				if event.Type == "response.done" {
					first, err := accounting.charge(ctx, s, budget, data)
					if err != nil {
						_ = writeSocketError(ctx, client, http.StatusServiceUnavailable, "Could not record Realtime token usage")
						return
					}
					if first {
						s.recordSocketUsage(request, candidate, alias, "realtime", data, budget)
					}
				}
			}
			if vad != nil {
				switch event.Type {
				case "session.created":
					if !providerSessionSeen {
						providerSessionSeen = true
						var fields map[string]any
						_ = json.Unmarshal(data, &fields)
						update := vad.initialize(fields, "gateway-vad-"+newUUID())
						if update != nil && upstream.Write(ctx, websocket.MessageText, update) != nil {
							return
						}
					}
				case "session.updated":
					var fields map[string]any
					_ = json.Unmarshal(data, &fields)
					if vad.initialUpdate != "" {
						config := getRealtimeVADConfig(fields)
						if config.detection["create_response"] == false {
							vad.initialUpdate = ""
							close(vad.ready)
							continue
						}
					} else {
						restored, err := vad.confirmUpdate(fields, "", false)
						if err != nil {
							_ = writeSocketError(ctx, client, http.StatusBadGateway, "Provider could not configure Realtime audio admission")
							return
						}
						if restored != nil {
							data = restored
						}
					}
				case "error":
					if vad.initialUpdate != "" && event.Error.EventID == vad.initialUpdate {
						_ = writeSocketError(ctx, client, http.StatusBadGateway, "Provider could not configure Realtime audio admission")
						return
					}
					_, _ = vad.confirmUpdate(nil, event.Error.EventID, true)
				}
			}
			if client.Write(ctx, kind, data) != nil {
				return
			}
			if vad != nil && vad.automaticTurn(event.Type, event.ItemID) {
				allowed, keepOpen := admitTurn(ctx)
				if !keepOpen || (allowed && upstream.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create"}`)) != nil) {
					return
				}
			}
		}
	})
}

func supportsNativeRealtimeSocket(provider string) bool {
	return provider == "openai" || provider == "azure" || provider == "xai"
}

func (s *Server) dialRealtimeSocket(ctx context.Context, candidate Candidate, intent, beta string) (*websocket.Conn, error) {
	if geminiLiveProvider(candidate) {
		return s.dialGeminiLive(ctx, candidate)
	}
	headers := make(http.Header)
	if candidate.Provider == "openai" && beta == "realtime=v1" {
		headers.Set("OpenAI-Beta", beta)
	}
	return s.dialProviderSocket(ctx, candidate, realtimeAddress(candidate, intent), headers)
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
	protocol := candidate.RealtimeProtocol
	if protocol == "" {
		protocol = os.Getenv("LITELLM_AZURE_REALTIME_PROTOCOL")
	}
	if strings.HasSuffix(base, "/openai/v1") || strings.EqualFold(protocol, "ga") || strings.EqualFold(protocol, "v1") {
		base = strings.TrimSuffix(strings.TrimSuffix(base, "/openai/v1"), "/openai")
		return base + "/openai/v1/realtime?" + query.Encode()
	}
	query.Del("model")
	query.Set("deployment", candidate.ModelID)
	version := candidate.APIVersion
	if version == "" {
		version = "2024-10-01-preview"
	}
	query.Set("api-version", version)
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
