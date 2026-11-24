package proxyconfig

import (
	"maps"
	"net/http"
	"time"
)

func (registry *KeyRegistry) resetKeySpend(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ResetTo *float64 `json:"reset_to"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil || input.ResetTo == nil ||
		*input.ResetTo < 0 {
		modelError(w, http.StatusBadRequest, "reset_to must be a non-negative number")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	caller, valid := registry.identifyResourceCallerLocked(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	digest := registry.resolveKey(r.PathValue("key"))
	key, exists := registry.state.Keys[digest]
	if !exists || key.RotationTarget != "" {
		modelError(w, http.StatusNotFound, "Virtual key not found")
		return
	}
	if !caller.admin {
		teamID := key.TeamID
		if teamID == "" {
			teamID = registry.state.Projects[key.ProjectID].TeamID
		}
		if !registry.canChangeTeamKeyLocked(caller, registry.state.Teams[teamID], "/key/{key_id}/reset_spend") {
			modelError(w, http.StatusForbidden, "Proxy or team administrator access required")
			return
		}
	}
	if registry.keySpend == nil {
		modelError(w, http.StatusInternalServerError, "Key spend reset requires PostgreSQL spend tracking")
		return
	}
	now := time.Now()
	start, reset, baseline, err := registry.keySpendWindowWithTier(r.Context(), key, now)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read key budget")
		return
	}
	identity := keyBudgetSpendIdentity(key, digest)
	recorded, err := registry.keySpend(r.Context(), identity, start)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read key spend")
		return
	}
	previous := max(0, baseline+recorded)
	if *input.ResetTo > previous || (key.MaxBudget != nil && *input.ResetTo > *key.MaxBudget) {
		modelError(w, http.StatusBadRequest, "reset_to cannot exceed current spend or the key budget")
		return
	}
	if key.BudgetID != "" {
		maximum, err := registry.budgetMaximum(r.Context(), key.BudgetID)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read linked key budget")
			return
		}
		if maximum != nil && *input.ResetTo > *maximum {
			modelError(w, http.StatusBadRequest, "reset_to cannot exceed the linked key budget")
			return
		}
	}
	base := importedKeySpend{}
	if key.ImportedSpend != nil {
		base = *key.ImportedSpend
	}
	base.Adjustment = &keySpendAdjustment{Start: start, Amount: keySpendDiscount(key, start) + previous - *input.ResetTo}
	key.ImportedSpend = &base
	next := registry.state
	next.Keys = maps.Clone(next.Keys)
	next.Keys[digest] = key
	before := map[string]any{"token": digest, "team_id": key.TeamID, "spend": previous}
	after := map[string]any{"token": digest, "team_id": key.TeamID, "spend": *input.ResetTo}
	if registry.apply(w, next, registry.entityAudit(r, "LiteLLM_VerificationToken", digest, "updated", before, after)) {
		modelJSON(w, http.StatusOK, map[string]any{
			"key_hash": digest, "spend": *input.ResetTo, "previous_spend": previous,
			"max_budget": key.MaxBudget, "budget_reset_at": reset,
		})
	}
}
