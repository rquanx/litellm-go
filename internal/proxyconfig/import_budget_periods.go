package proxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

func decodeImportedBudgetWindows(raw json.RawMessage, snapshot time.Time) ([]keyBudgetLimit, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, err
		}
		raw = json.RawMessage(encoded)
	}
	var source []struct {
		Duration string `json:"budget_duration"`
		Max *float64 `json:"max_budget"`
		Reset json.RawMessage `json:"reset_at"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, errors.New("invalid LiteLLM budget windows")
	}
	seen := make(map[string]bool, len(source))
	var limits []keyBudgetLimit
	for _, window := range source {
		reset, err := liteLLMTimestamp(window.Reset)
		if err != nil || window.Max == nil || *window.Max < 0 || seen[window.Duration] {
			return nil, errors.New("invalid LiteLLM budget window limit or reset")
		}
		next, err := nextBudgetReset(snapshot, window.Duration)
		if err != nil {
			return nil, err
		}
		if reset == nil {
			reset = &next
		}
		seen[window.Duration] = true
		limits = append(limits, keyBudgetLimit{Duration: window.Duration, Max: window.Max, ResetAt: reset, StartsAt: &snapshot})
	}
	return limits, nil
}

func importBudgetWindowBalances(ctx context.Context, state *accessState, source *redis.Client, namespace string, snapshot time.Time) error {
	var windows []*keyBudgetLimit
	var names []string
	if namespace != "" {
		namespace += ":"
	}
	collect := func(owner string, limits []keyBudgetLimit) {
		for i := range limits {
			window := &limits[i]
			windows = append(windows, window)
			names = append(names, namespace+"spend:"+owner+":window:"+window.Duration)
		}
	}
	for digest, key := range state.Keys {
		collect("key:"+digest, key.BudgetLimits)
	}
	for id, team := range state.Teams {
		collect("team:"+id, team.BudgetLimits)
	}
	if len(names) == 0 {
		return nil
	}
	if source == nil {
		return errors.New("LiteLLM budget windows require LITELLM_GO_IMPORT_REDIS_URL")
	}
	for start := 0; start < len(names); start += 256 {
		end := min(start+256, len(names))
		values, err := source.MGet(ctx, names[start:end]...).Result()
		if err != nil {
			return errors.New("could not read source Redis budget counters")
		}
		for i, value := range values {
			balance := 0.0
			if value != nil {
				raw, ok := value.(string)
				if !ok {
					return errors.New("invalid source Redis budget counter")
				}
				balance, err = strconv.ParseFloat(raw, 64)
				if err != nil || math.IsNaN(balance) || math.IsInf(balance, 0) || balance < 0 {
					return errors.New("invalid source Redis budget counter")
				}
			}
			window := windows[start+i]
			if !snapshot.Before(*window.ResetAt) {
				// The source's scheduled reset may not have flushed the old key yet.
				continue
			}
			window.ImportedSpend = &importedKeySpend{Amount: balance, AsOf: snapshot, ResetAt: window.ResetAt}
		}
	}
	return nil
}
