package proxyconfig

import (
	"maps"
	"net/http"
	"os"
	"strings"
	"time"
)

func (registry *KeyRegistry) regenerateKey(w http.ResponseWriter, r *http.Request) {
	if !registry.requireMaster(w, r) {
		return
	}
	var input struct {
		Key         string  `json:"key"`
		NewKey      string  `json:"new_key"`
		Duration    *string `json:"duration"`
		GracePeriod *string `json:"grace_period"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.Key == "" {
		modelError(w, http.StatusBadRequest, "Invalid or unsupported key rotation")
		return
	}
	now := time.Now().UTC()
	gracePeriod := os.Getenv("LITELLM_KEY_ROTATION_GRACE_PERIOD")
	if input.GracePeriod != nil {
		gracePeriod = *input.GracePeriod
	}
	graceExpiry, err := keyExpiry(now, gracePeriod)
	if err != nil || gracePeriod == "-1" {
		modelError(w, http.StatusBadRequest, "Invalid key rotation grace period")
		return
	}
	token := input.NewKey
	if token == "" {
		token = newKeyToken()
	}
	if !strings.HasPrefix(token, "sk-") || len(token) < 8 || len(token) > 512 ||
		strings.ContainsAny(token, " \t\r\n") || token == registry.master {
		modelError(w, http.StatusBadRequest, "Invalid replacement key")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	oldID := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[oldID]
	if !exists || key.RotationTarget != "" {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	newID := keyDigest(token)
	if _, exists := registry.state.Keys[newID]; exists {
		modelError(w, http.StatusConflict, "Replacement key already exists")
		return
	}
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	old := key
	if input.Duration != nil {
		expires, err := keyExpiry(now, *input.Duration)
		if err != nil {
			modelError(w, http.StatusBadRequest, "Invalid key duration")
			return
		}
		key.Expires = expires
	}
	delete(next.Keys, oldID)
	for id, grace := range next.Keys {
		if grace.RotationTarget == oldID {
			delete(next.Keys, id)
		}
	}
	if graceExpiry != nil && graceExpiry.After(now) {
		if old.Expires == nil || graceExpiry.Before(*old.Expires) {
			old.Expires = graceExpiry
		}
		old.RotationTarget = newID
		next.Keys[oldID] = old
	}
	next.Keys[newID] = key
	if registry.apply(w, next) {
		modelJSON(w, http.StatusOK, map[string]any{
			"key": token, "token_id": newID, "expires": key.Expires, "key_alias": key.Alias,
		})
	}
}
