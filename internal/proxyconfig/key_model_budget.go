package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type keyModelBudget struct {
	Max *float64 `json:"max_budget,omitempty"`
	Duration string `json:"budget_duration,omitempty"`
	BudgetLimit *float64 `json:"budget_limit,omitempty"`
	TimePeriod *string `json:"time_period,omitempty"`
	RPM *int `json:"rpm_limit,omitempty"`
	TPM *int `json:"tpm_limit,omitempty"`
}

func (budget keyModelBudget) spendConfig() (*float64, string) {
	maximum, duration := budget.Max, budget.Duration
	if budget.BudgetLimit != nil {
		maximum = budget.BudgetLimit
	}
	if budget.TimePeriod != nil {
		duration = *budget.TimePeriod
	}
	return maximum, duration
}

func validateKeyModelBudgets(budgets map[string]keyModelBudget, hasSpendStore bool) error {
	for model, budget := range budgets {
		maximum, duration := budget.spendConfig()
		if strings.TrimSpace(model) == "" || (maximum != nil && *maximum < 0) ||
			(budget.RPM != nil && *budget.RPM < 0) || (budget.TPM != nil && *budget.TPM < 0) {
			return errors.New("model_max_budget requires model names and non-negative limits")
		}
		if duration != "" && modelBudgetExpiration(time.Now().UTC(), duration).IsZero() {
			return errors.New("model_max_budget contains an invalid budget_duration")
		}
		if maximum != nil && *maximum > 0 && duration != "" && !hasSpendStore {
			return errors.New("model_max_budget spend limits require PostgreSQL spend tracking")
		}
	}
	return nil
}

func modelBudgetExpiration(at time.Time, duration string) time.Time {
	match := budgetDurationPattern.FindStringSubmatch(duration)
	if match == nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if match[2] == "mo" {
		if n > int64((9999-at.Year())*12+12-int(at.Month())) {
			return time.Time{}
		}
		month := at.AddDate(0, int(n), 1-at.Day())
		day := min(at.Day(), month.AddDate(0, 1, -1).Day())
		return month.AddDate(0, 0, day-1)
	}
	units := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}
	expiration := time.Unix(at.Unix()+n*units[match[2]], int64(at.Nanosecond())).UTC()
	if expiration.Year() > 9999 {
		return time.Time{}
	}
	return expiration
}

func keyModelPolicy(key virtualKey, identity string, log *SpendLog) func(context.Context, string) (runtime.KeyModelPolicy, error) {
	var db *sql.DB
	if log != nil {
		db = log.db
	}
	return modelBudgetPolicy(key.ModelBudgets, db, identity, "Virtual Key")
}

func customerModelBudgetIdentity(id string) string {
	return "customer:" + id
}

func modelBudgetPolicy(budgets map[string]keyModelBudget, db *sql.DB, identity, owner string) func(context.Context, string) (runtime.KeyModelPolicy, error) {
	if len(budgets) == 0 {
		return nil
	}
	return func(ctx context.Context, model string) (runtime.KeyModelPolicy, error) {
		budget, found := budgets[model]
		if !found {
			if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
				budget, found = budgets[model[slash+1:]]
			}
		}
		if !found {
			return runtime.KeyModelPolicy{}, nil
		}
		var policy runtime.KeyModelPolicy
		maximum, duration := budget.spendConfig()
		if maximum == nil || *maximum == 0 || duration == "" {
			return policy, nil
		}
		if db == nil {
			return policy, errors.New("model budget spend tracking is disabled")
		}
		policy.SpendDuration = duration
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var spend float64
		err := db.QueryRowContext(ctx, `SELECT spend FROM minato_proxy_key_model_spend
			WHERE key_identity=$1 AND model_group=$2 AND duration=$3 AND reset_at > clock_timestamp()`,
			identity, model, duration).Scan(&spend)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return policy, err
		}
		if spend > *maximum {
			policy.BudgetError = fmt.Sprintf("LiteLLM %s exceeded budget for model=%s. Current cost: %g, Max budget: %g", owner, model, spend, *maximum)
		}
		return policy, nil
	}
}
