package runtime

import (
	"context"
	"encoding/json"
	"time"
)

func socketTokenLimits(budget KeyBudget) []OwnerRateLimits {
	limits := append([]OwnerRateLimits{{Identity: budget.SpendIdentity, TPM: budget.TPMLimit}}, budget.OwnerLimits...)
	return limits
}

func (s *Server) checkRealtimeTokens(ctx context.Context, budget KeyBudget) (bool, error) {
	if !budget.hasTokenLimit() {
		return true, nil
	}
	for _, limit := range socketTokenLimits(budget) {
		if limit.TPM == nil {
			continue
		}
		release, retry, err := s.reserveKeyTokens(ctx, limit.Identity, *limit.TPM, 1, time.Now().Unix())
		if err != nil || retry > 0 {
			return false, err
		}
		release(0)
	}
	return true, nil
}

type realtimeTokenUsage struct {
	recent [128]string
	next int
}

func (usage *realtimeTokenUsage) charge(ctx context.Context, s *Server, budget KeyBudget, body []byte) (bool, error) {
	var event struct {
		Response struct{ ID string } `json:"response"`
	}
	if json.Unmarshal(body, &event) != nil || event.Response.ID == "" {
		return false, nil
	}
	for _, id := range usage.recent {
		if event.Response.ID == id {
			return false, nil
		}
	}
	used := usageFromJSON(body)
	if used > 0 && budget.hasTokenLimit() {
		for _, limit := range socketTokenLimits(budget) {
			if limit.TPM == nil {
				continue
			}
			// Reconcile completed usage even if the turn exceeded its allowance.
			_, _, err := s.reserveKeyTokens(ctx, limit.Identity, int(^uint(0)>>1), used, time.Now().Unix())
			if err != nil {
				return false, err
			}
		}
	}
	usage.recent[usage.next] = event.Response.ID
	usage.next = (usage.next + 1) % len(usage.recent)
	return true, nil
}
