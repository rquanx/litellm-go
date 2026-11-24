package proxyconfig

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
)

type providerBudget struct {
	Maximum float64 `json:"max_budget" yaml:"max_budget"`
	Duration string `json:"budget_duration" yaml:"budget_duration"`
	anchor time.Time
}

type providerBudgetStatus struct {
	Maximum float64 `json:"budget_limit"`
	Duration string `json:"time_period"`
	Spend float64 `json:"spend"`
	Reset *time.Time `json:"budget_reset_at"`
}

func (log *SpendLog) ConfigureProviderBudgets(ctx context.Context, config Config) error {
	if len(config.ProviderBudgets) == 0 {
		return nil
	}
	if _, err := log.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_provider_budget_window (
		provider TEXT PRIMARY KEY, duration TEXT NOT NULL, anchor TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_provider_started ON minato_proxy_spend (provider, started_at)`); err != nil {
		return err
	}
	log.providerBudgets = make(map[string]providerBudget, len(config.ProviderBudgets))
	for name, budget := range config.ProviderBudgets {
		next, err := nextBudgetReset(time.Now().In(budgetZone(config.BudgetTimezone)), budget.Duration)
		if err != nil {
			return err
		}
		if err := log.db.QueryRowContext(ctx, `INSERT INTO minato_provider_budget_window (provider,duration,anchor) VALUES ($1,$2,$3)
			ON CONFLICT (provider) DO UPDATE SET duration=EXCLUDED.duration,
			anchor=CASE WHEN minato_provider_budget_window.duration=EXCLUDED.duration
				THEN minato_provider_budget_window.anchor ELSE EXCLUDED.anchor END
			RETURNING anchor`, name, budget.Duration, next).Scan(&budget.anchor); err != nil {
			return err
		}
		log.providerBudgets[name] = budget
	}
	return nil
}

func validateProviderBudgets(budgets map[string]providerBudget) error {
	for name, budget := range budgets {
		if strings.TrimSpace(name) == "" || budget.Maximum < 0 || math.IsNaN(budget.Maximum) || math.IsInf(budget.Maximum, 0) {
			return errors.New("invalid provider budget")
		}
		if _, err := nextBudgetReset(time.Now(), budget.Duration); err != nil {
			return errors.New("invalid provider budget duration")
		}
	}
	return nil
}

func (log *SpendLog) providerBudgetStatus(ctx context.Context, name string, budget providerBudget) (providerBudgetStatus, error) {
	start, reset := log.keys.budgetWindow(budget.Duration, &budget.anchor, time.Now())
	result := providerBudgetStatus{Maximum: budget.Maximum, Duration: budget.Duration, Reset: reset}
	err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend),0) FROM minato_proxy_spend
		WHERE provider=$1 AND started_at >= $2`, name, start).Scan(&result.Spend)
	return result, err
}

func (log *SpendLog) CheckProviderBudget(ctx context.Context, provider string) bool {
	budget, configured := log.providerBudgets[provider]
	if !configured {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	status, err := log.providerBudgetStatus(ctx, provider, budget)
	return err == nil && status.Spend < status.Maximum
}

func (log *SpendLog) listProviderBudgets(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	entries := make(map[string]providerBudgetStatus, len(log.providerBudgets))
	for name, budget := range log.providerBudgets {
		status, err := log.providerBudgetStatus(ctx, name, budget)
		if err != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not read provider budgets")
			return
		}
		entries[name] = status
	}
	modelJSON(w, http.StatusOK, map[string]any{"providers": entries})
}

