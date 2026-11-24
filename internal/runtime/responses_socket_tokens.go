package runtime

import (
	"context"
	"sync"
	"time"
)

// Responses delivers terminal events in turn order on a session.
type responseSocketTokens struct {
	mu sync.Mutex
	pending []responseSocketTurn
}

type responseSocketTurn struct {
	budget KeyBudget
	reservations []func(int)
}

func (tokens *responseSocketTokens) reserve(ctx context.Context, server *Server, budget KeyBudget, estimated int) (bool, error) {
	limits := socketTokenLimits(budget)
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	if len(tokens.pending) >= 128 {
		return false, nil
	}
	var reservations []func(int)
	for _, limit := range limits {
		if limit.TPM == nil {
			continue
		}
		settle, retry, err := server.reserveKeyTokens(ctx, limit.Identity, *limit.TPM, estimated, time.Now().Unix())
		if err != nil || retry > 0 {
			for _, release := range reservations {
				release(0)
			}
			return false, err
		}
		reservations = append(reservations, settle)
	}
	budget.ModelPolicy, budget.ModelRates, budget.RouterSettings = nil, nil, nil
	tokens.pending = append(tokens.pending, responseSocketTurn{budget: budget, reservations: reservations})
	return true, nil
}

func (tokens *responseSocketTokens) settle(actual int) (KeyBudget, bool) {
	tokens.mu.Lock()
	if len(tokens.pending) == 0 {
		tokens.mu.Unlock()
		return KeyBudget{}, false
	}
	first := tokens.pending[0]
	tokens.pending[0] = responseSocketTurn{}
	tokens.pending = tokens.pending[1:]
	tokens.mu.Unlock()
	for _, settle := range first.reservations {
		settle(actual)
	}
	return first.budget, true
}

func (tokens *responseSocketTokens) close() {
	// The relay has exited in both directions, so no new reservations arrive.
	for len(tokens.pending) > 0 {
		tokens.settle(0)
	}
}
