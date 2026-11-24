package proxyconfig

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"regexp"
	"strconv"
	"time"
	_ "time/tzdata"
)

var budgetDurationPattern = regexp.MustCompile("^([0-9]+)(mo|[smhdw])$")

func budgetZone(name string) *time.Location {
	zone, err := time.LoadLocation(name)
	if err != nil || name == "" || name == "Local" {
		return time.UTC
	}
	return zone
}

func nextBudgetReset(now time.Time, duration string) (time.Time, error) {
	parts := budgetDurationPattern.FindStringSubmatch(duration)
	if parts == nil {
		return time.Time{}, errors.New("invalid budget duration")
	}
	n, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || n <= 0 {
		return time.Time{}, errors.New("invalid budget duration")
	}
	count := int(n)
	year, month, day := now.Date()
	hour, minute, second := now.Clock()
	zone := now.Location()
	var next time.Time
	switch parts[2] {
	case "mo":
		if count != 1 {
			return time.Time{}, errors.New("monthly budgets require a one-month interval")
		}
		next = time.Date(year, month+1, 1, 0, 0, 0, 0, zone)
	case "d", "w":
		switch {
		case parts[2] == "d" && count == 30:
			next = time.Date(year, month+1, 1, 0, 0, 0, 0, zone)
		default:
			days := int64(count)
			if parts[2] == "w" || count == 7 {
				days = int64((7-int(now.Weekday())+int(time.Monday))%7)
				if days == 0 {
					days = 7
				}
				if parts[2] == "w" {
					days += (n - 1) * 7
				}
			}
			if days > 3_000_000 {
				return time.Time{}, errors.New("budget duration is too large")
			}
			next = time.Date(year, month, day+int(days), 0, 0, 0, 0, zone)
		}
	case "h":
		next = time.Date(year, month, day, hour+count-hour%count, 0, 0, 0, zone)
	case "m":
		next = time.Date(year, month, day, hour, minute+count-minute%count, 0, 0, zone)
	case "s":
		next = time.Date(year, month, day, hour, minute, second+count-second%count, 0, zone)
	}
	if !next.After(now) {
		// A wall-clock boundary may be skipped by a daylight-saving change.
		next = next.Add(time.Hour)
	}
	if !next.After(now) || next.Year() > 9999 {
		return time.Time{}, errors.New("invalid budget reset time")
	}
	return next.UTC(), nil
}

func (registry *CustomerRegistry) RunBudgetResets(ctx context.Context, config Config) {
	zone := budgetZone(config.BudgetTimezone)
	minimum, maximum := config.budgetResetMin, config.budgetResetMax
	if minimum == 0 {
		minimum, maximum = 597, 605
	}
	seconds := minimum + rand.IntN(min(30, maximum-minimum)+1)
	ticker := time.NewTicker(time.Duration(seconds) * time.Second)
	defer ticker.Stop()
	for {
		resetCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := registry.resetExpiredBudgets(resetCtx, time.Now().In(zone))
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("could not reset customer budgets: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (registry *CustomerRegistry) resetExpiredBudgets(ctx context.Context, now time.Time) error {
	rows, err := registry.db.QueryContext(ctx, `SELECT budget_id,budget_duration,budget_reset_at
		FROM `+budgetTable+` WHERE budget_duration IS NOT NULL AND budget_reset_at <= $1`, now.UTC())
	if err != nil {
		return err
	}
	defer rows.Close()
	type dueBudget struct {
		id, duration string
		reset        time.Time
	}
	var due []dueBudget
	for rows.Next() {
		var entry dueBudget
		if err := rows.Scan(&entry.id, &entry.duration, &entry.reset); err != nil {
			return err
		}
		due = append(due, entry)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var firstErr error
	for _, budget := range due {
		next, err := nextBudgetReset(now, budget.duration)
		if err == nil {
			// Advance the reset and clear linked spend in one statement. A second
			// instance with the same snapshot no longer matches the old reset.
			_, err = registry.db.ExecContext(ctx, `WITH advanced AS (
				UPDATE `+budgetTable+` SET budget_reset_at=$4, budget_period_started_at=$5
				WHERE budget_id=$1 AND budget_duration=$2 AND budget_reset_at=$3 RETURNING budget_id
			)
			, cleared_customers AS (
				UPDATE `+customerTable+` AS customer SET spend=0 FROM advanced
				WHERE customer.budget_id=advanced.budget_id AND customer.spend<>0
			)
			UPDATE `+tagTable+` AS tag SET spend=0 FROM advanced
			WHERE tag.budget_id=advanced.budget_id AND tag.spend<>0`,
				budget.id, budget.duration, budget.reset, next, now.UTC())
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
