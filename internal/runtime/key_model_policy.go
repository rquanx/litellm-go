package runtime

import (
	"context"
	"net/http"
)

type KeyModelPolicy struct {
	SpendDuration string
	BudgetError string
}

func (s *Server) checkKeyModelPolicy(w http.ResponseWriter, r *http.Request,
	lookup func(context.Context, string) (KeyModelPolicy, error), model string) (KeyModelPolicy, bool) {
	policy, err := lookup(r.Context(), model)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "budget_lookup_failed", "Could not read key model budget")
		return policy, false
	}
	if policy.BudgetError != "" {
		writeBudgetExceeded(w, policy.BudgetError)
		return policy, false
	}
	return policy, true
}
