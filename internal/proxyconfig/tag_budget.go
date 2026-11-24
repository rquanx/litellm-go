package proxyconfig

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
)

const tagTable = `"LiteLLM_TagTable"`

func openTagTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+tagTable+` (
		tag_name TEXT PRIMARY KEY, description TEXT, models TEXT[] NOT NULL DEFAULT '{}',
		model_info JSONB, spend DOUBLE PRECISION NOT NULL DEFAULT 0, budget_id TEXT,
		created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
		created_by TEXT, updated_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

func (registry *CustomerRegistry) ResolveDeploymentTags(ctx context.Context, tags []string) (map[string][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := registry.db.QueryContext(ctx, `SELECT tag_name,unnest(models) FROM `+tagTable+` WHERE tag_name=ANY($1)`, pq.Array(tags))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := make(map[string][]string)
	for rows.Next() {
		var tag, model string
		if err := rows.Scan(&tag, &model); err != nil {
			return nil, err
		}
		if model != "" {
			models[model] = append(models[model], tag)
		}
	}
	return models, rows.Err()
}

func (log *SpendLog) CheckTagBudget(ctx context.Context, tags []string) (string, error) {
	if len(tags) == 0 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var tag string
	var spend, maximum float64
	err := log.db.QueryRowContext(ctx, `SELECT t.tag_name,t.spend,b.max_budget FROM `+tagTable+` t
		JOIN `+budgetTable+` b ON b.budget_id=t.budget_id WHERE t.tag_name=ANY($1) AND t.spend>b.max_budget
		ORDER BY array_position($1,t.tag_name) LIMIT 1`, pq.Array(tags)).Scan(&tag, &spend, &maximum)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Budget has been exceeded! Tag=%s Current cost: %g, Max budget: %g", tag, spend, maximum), nil
}
