package proxyconfig

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"
)

func (registry *KeyRegistry) bulkUpdateTeamKeys(w http.ResponseWriter, r *http.Request) {
	const maxKeys = 500
	var input struct {
		TeamID string `json:"team_id"`
		KeyIDs []string `json:"key_ids"`
		All bool `json:"all_keys_in_team"`
		Update json.RawMessage `json:"update_fields"`
	}
	var fields struct {
		BudgetID json.RawMessage `json:"budget_id"`
		MaxBudget json.RawMessage `json:"max_budget"`
		BudgetDuration json.RawMessage `json:"budget_duration"`
		BudgetLimits json.RawMessage `json:"budget_limits"`
		ModelBudgets json.RawMessage `json:"model_max_budget"`
		RPM json.RawMessage `json:"rpm_limit"`
		TPM json.RawMessage `json:"tpm_limit"`
		Parallel json.RawMessage `json:"max_parallel_requests"`
		Duration json.RawMessage `json:"duration"`
		Tags *[]string `json:"tags"`
		Metadata *map[string]any `json:"metadata"`
	}
	var rawFields map[string]json.RawMessage
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
		input.TeamID == "" || input.All == (len(input.KeyIDs) > 0) || len(input.KeyIDs) > maxKeys ||
		json.Unmarshal(input.Update, &rawFields) != nil || len(rawFields) == 0 ||
		decodeModelJSON(bytes.NewReader(input.Update), &fields) != nil {
		modelError(w, http.StatusBadRequest, "Supply a team, up to 500 key IDs or all_keys_in_team, and supported update fields")
		return
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	caller, ok := registry.identifyResourceCallerLocked(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	team, exists := registry.state.Teams[input.TeamID]
	if !caller.admin {
		if !registry.canChangeTeamKeyLocked(caller, team, "/key/update") {
			modelError(w, http.StatusUnauthorized, "Team key update permission required")
			return
		}
		if _, updatesMetadata := rawFields["metadata"]; updatesMetadata {
			modelError(w, http.StatusForbidden, "Only proxy admins can replace key metadata")
			return
		}
	}
	if !exists {
		modelError(w, http.StatusNotFound, "Team not found")
		return
	}

	references := input.KeyIDs
	if input.All {
		now := time.Now()
		for digest, key := range registry.state.Keys {
			if key.TeamID == input.TeamID && key.RotationTarget == "" && !key.Blocked &&
				(key.Expires == nil || now.Before(*key.Expires)) {
				references = append(references, digest)
				if len(references) > maxKeys {
					modelError(w, http.StatusBadRequest, "Select at most 500 key IDs")
					return
				}
			}
		}
		slices.Sort(references)
	}
	if len(references) == 0 {
		modelError(w, http.StatusNotFound, "No keys found for team")
		return
	}

	update := keyUpdate{BudgetID: fields.BudgetID, MaxBudget: fields.MaxBudget, BudgetDuration: fields.BudgetDuration,
		BudgetLimits: fields.BudgetLimits, ModelBudgets: fields.ModelBudgets, RPMLimit: fields.RPM, TPMLimit: fields.TPM,
		ParallelLimit: fields.Parallel, Duration: fields.Duration, Tags: fields.Tags, Metadata: fields.Metadata}
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	seen := make(map[string]bool, len(references))
	var events []*managementAuditEvent
	successful, failed := []bulkKeyResult{}, []bulkKeyResult{}
	for _, reference := range references {
		digest := registry.resolveKey(reference)
		if seen[digest] {
			continue
		}
		seen[digest] = true
		key, exists := next.Keys[digest]
		if !exists || key.TeamID != input.TeamID || key.RotationTarget != "" {
			failed = append(failed, bulkKeyResult{Key: reference, Reason: "Key not found in team"})
			continue
		}
		if err := registry.applyKeyChanges(r.Context(), &key, update); err != nil {
			failed = append(failed, bulkKeyResult{Key: reference, Reason: err.Error()})
			continue
		}
		if registry.storeAuditLogs {
			before := registry.state.Keys[digest]
			events = append(events, registry.keyAudit(r, digest, "updated", &before, &key))
		}
		next.Keys[digest] = key
		key.ImportedSpend = nil
		key.BudgetLimits = publicBudgetLimits(key.BudgetLimits)
		successful = append(successful, bulkKeyResult{Key: reference, Info: &key})
	}
	if len(successful) > 0 && !registry.apply(w, next, events...) {
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{
		"total_requested": len(seen), "successful_updates": successful, "failed_updates": failed,
	})
}

