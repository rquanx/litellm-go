package proxyconfig

import (
	"context"
	"fmt"
	"time"
)

func (log *SpendLog) ConfigureGlobalBudget(ctx context.Context, config Config) error {
	if config.GlobalMaxBudget == 0 {
		return nil
	}
	next, err := nextBudgetReset(time.Now().In(budgetZone(config.BudgetTimezone)), config.GlobalBudgetDuration)
	if err != nil {
		return err
	}
	if _, err := log.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_proxy_budget_window (
		singleton BOOLEAN PRIMARY KEY CHECK (singleton),
		duration TEXT NOT NULL,
		anchor TIMESTAMPTZ NOT NULL
	)`); err != nil {
		return err
	}
	if err := log.db.QueryRowContext(ctx, `INSERT INTO minato_proxy_budget_window VALUES (true,$1,$2)
		ON CONFLICT (singleton) DO UPDATE SET duration=EXCLUDED.duration,
		anchor=CASE WHEN minato_proxy_budget_window.duration=EXCLUDED.duration
			THEN minato_proxy_budget_window.anchor ELSE EXCLUDED.anchor END
		RETURNING anchor`, config.GlobalBudgetDuration, next).Scan(&log.globalBudgetAnchor); err != nil {
		return err
	}
	log.maxBudget = config.GlobalMaxBudget
	log.globalBudgetDuration = config.GlobalBudgetDuration
	log.disableGlobalBudgetReset = config.DisableResetBudget
	return nil
}

func (log *SpendLog) CheckGlobalBudget(ctx context.Context) (string, error) {
	start := time.Time{}
	if !log.disableGlobalBudgetReset {
		start, _ = log.keys.budgetWindow(log.globalBudgetDuration, &log.globalBudgetAnchor, time.Now())
	}
	start = log.effectiveSpendStart(start)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var total float64
	err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend),0) FROM minato_proxy_spend WHERE started_at >= $1`, start).Scan(&total)
	if err != nil {
		return "", err
	}
	if total > log.maxBudget {
		return fmt.Sprintf("Budget has been exceeded! Current cost: %g, Max budget: %g", total, log.maxBudget), nil
	}
	return "", nil
}
