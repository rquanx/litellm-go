package proxyconfig

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type teamCallbackRunner struct {
	fingerprint string
	send        func(runtime.InferenceEvent)
}

// TeamCallback returns the callback dispatcher used for per-team logging.
// Team callback settings are stored in team metadata by the management API.
// A runner is cached per team so normal inference does not create a worker for
// every request.
func (registry *KeyRegistry) TeamCallback(ctx context.Context) func(runtime.InferenceEvent) {
	var mu sync.Mutex
	runners := make(map[string]teamCallbackRunner)
	return func(event runtime.InferenceEvent) {
		if event.SpendAttribution == nil || event.SpendAttribution.TeamID == "" {
			return
		}
		teamID := event.SpendAttribution.TeamID
		fingerprint, callbacks := registry.teamCallbacks(teamID)
		if len(callbacks) == 0 {
			return
		}
		mu.Lock()
		runner, exists := runners[teamID]
		if !exists || runner.fingerprint != fingerprint {
			prepared := make(GenericCallbacks, 0, len(callbacks))
			for _, callback := range callbacks {
				if callback.endpoint == "" {
					continue
				}
				prepared = append(prepared, callback)
			}
			if len(prepared) == 0 {
				mu.Unlock()
				return
			}
			runner = teamCallbackRunner{fingerprint: fingerprint, send: prepared.Start(ctx)}
			runners[teamID] = runner
		}
		mu.Unlock()
		runner.send(event)
	}
}

func (registry *KeyRegistry) teamCallbacks(teamID string) (string, []*GenericCallback) {
	registry.mu.RLock()
	metadata := registry.state.Teams[teamID].Metadata
	registry.mu.RUnlock()
	entries, ok := metadata["logging"].([]any)
	if !ok || len(entries) == 0 {
		return "", nil
	}
	fingerprintBytes, _ := json.Marshal(entries)
	callbacks := make([]*GenericCallback, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["callback_name"].(string)
		typ, _ := entry["callback_type"].(string)
		vars, _ := entry["callback_vars"].(map[string]any)
		endpoint := callbackString(vars, "endpoint", "url", "generic_logger_endpoint")
		if endpoint == "" || name == "" {
			continue
		}
		headers := make(map[string]string)
		if values, ok := vars["headers"].(map[string]any); ok {
			for key, value := range values {
				if text, ok := value.(string); ok {
					headers[key] = text
				}
			}
		}
		callbacks = append(callbacks, &GenericCallback{
			name: name, endpoint: endpoint, format: "json_array", headers: headers,
			success: typ == "success" || typ == "success_and_failure",
			failure: typ == "failure" || typ == "success_and_failure",
		})
	}
	return string(fingerprintBytes), callbacks
}

func callbackString(values map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := values[name].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

