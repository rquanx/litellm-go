package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (callbacks GenericCallbacks) keyHealth(w http.ResponseWriter, r *http.Request, keys *KeyRegistry) {
	metadata, authorized := keys.keyHealthMetadata(runtime.ProxyCredential(r))
	if !authorized {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	raw, hasLogging := metadata["logging"]
	if !hasLogging {
		modelJSON(w, http.StatusOK, map[string]any{"key": "healthy", "logging_callbacks": nil})
		return
	}
	var logging []struct {
		Name string `json:"callback_name"`
	}
	data, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(data, &logging) != nil {
		modelError(w, http.StatusBadRequest, "Invalid key logging configuration")
		return
	}
	seen := make(map[string]bool)
	names := make([]string, 0, len(logging))
	for _, entry := range logging {
		if entry.Name == "" {
			modelError(w, http.StatusBadRequest, "Key logging callback_name is required")
			return
		}
		if !seen[entry.Name] {
			names = append(names, entry.Name)
			seen[entry.Name] = true
		}
	}
	if len(names) > 16 {
		modelError(w, http.StatusBadRequest, "Too many key logging callbacks")
		return
	}

	now := float64(time.Now().UnixNano()) / float64(time.Second)
	event := runtime.InferenceEvent{
		ID: newEntityID(), CallType: "acompletion", Model: "litellm-key-health-test", Provider: "openai",
		Status: "success", StartTime: now, EndTime: now, HTTPStatus: http.StatusOK,
		Messages: json.RawMessage(`[{"role":"user","content":"Synthetic LiteLLM key health probe"}]`),
		Response: json.RawMessage(`{"choices":[{"message":{"role":"assistant","content":"Test response"}}]}`),
	}
	event.TraceID = event.ID
	status, details := "healthy", "All configured callback probes succeeded"
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	for _, name := range names {
		if !callbacks.Probe(ctx, name, event) {
			status, details = "unhealthy", "A configured callback is unavailable or rejected the probe"
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"key": status, "logging_callbacks": map[string]any{
		"callbacks": names, "status": status, "details": details,
	}})
}

func (keys *KeyRegistry) keyHealthMetadata(token string) (map[string]any, bool) {
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	if keys.master != "" && token == keys.master {
		return nil, true
	}
	if _, _, ok := keys.authorizeLocked(token); !ok {
		return nil, false
	}
	key := keys.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key = keys.state.Keys[key.RotationTarget]
	}
	return key.Metadata, true
}

