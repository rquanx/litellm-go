package proxyconfig

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
)

type bulkKeyResult struct {
	Key    string      `json:"key"`
	Info   *virtualKey `json:"key_info"`
	Reason string      `json:"failed_reason,omitempty"`
}

func (registry *KeyRegistry) bulkUpdateKeys(w http.ResponseWriter, r *http.Request) {
	caller, valid := registry.identifyResourceCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin {
		modelError(w, http.StatusForbidden, "Only proxy admins can perform bulk key updates")
		return
	}
	var input struct {
		Keys []struct {
			Key       string          `json:"key"`
			TeamID    *string         `json:"team_id"`
			BudgetID  json.RawMessage `json:"budget_id"`
			MaxBudget json.RawMessage `json:"max_budget"`
			Tags      *[]string       `json:"tags"`
		} `json:"keys"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		len(input.Keys) == 0 || len(input.Keys) > 500 {
		modelError(w, http.StatusBadRequest, "Supply between 1 and 500 key updates")
		return
	}
	successful, failed := []bulkKeyResult{}, []bulkKeyResult{}
	for _, item := range input.Keys {
		result := registry.applyBulkKeyItem(r, keyUpdate{
			Key: item.Key, TeamID: item.TeamID, BudgetID: item.BudgetID, MaxBudget: item.MaxBudget, Tags: item.Tags,
		})
		if result.Reason != "" {
			failed = append(failed, result)
		} else {
			successful = append(successful, result)
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"total_requested": len(input.Keys), "successful_updates": successful, "failed_updates": failed,
	})
}

func (registry *KeyRegistry) applyBulkKeyItem(r *http.Request, input keyUpdate) bulkKeyResult {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	result := bulkKeyResult{Key: input.Key}
	digest := registry.resolveKey(input.Key)
	key, exists := registry.state.Keys[digest]
	if !exists || key.RotationTarget != "" {
		result.Reason = "Virtual key not found"
		return result
	}
	public := key
	public.ImportedSpend = nil
	public.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
	result.Info = &public
	if err := registry.applyKeyChanges(r.Context(), &key, input); err != nil {
		result.Reason = err.Error()
		return result
	}
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	next.Keys[digest] = key
	if err := registry.commitState(next, registry.keyAudit(r, digest, "updated", &public, &key)); err != nil {
		result.Reason = "Could not save proxy state"
		if errors.Is(err, errConcurrentState) {
			result.Reason = "Proxy state was changed by another process"
		}
		return result
	}
	public = key
	public.ImportedSpend = nil
	public.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
	return result
}
