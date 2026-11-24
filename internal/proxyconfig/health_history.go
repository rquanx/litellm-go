package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

type healthHistory struct {
	db *sql.DB
}

func OpenHealthHistory(ctx context.Context, db *sql.DB) (runtime.HealthHistory, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_proxy_health (
		id TEXT PRIMARY KEY,
		model_name TEXT NOT NULL,
		model_id TEXT NOT NULL,
		status TEXT NOT NULL,
		healthy_count INTEGER NOT NULL,
		unhealthy_count INTEGER NOT NULL,
		response_ms DOUBLE PRECISION NOT NULL,
		checked_at TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS minato_proxy_health_checked_at ON minato_proxy_health (checked_at DESC, id);
	CREATE INDEX IF NOT EXISTS minato_proxy_health_latest ON minato_proxy_health (model_id, checked_at DESC, id);`)
	if err != nil {
		return nil, errors.New("could not initialize Go proxy health history")
	}
	return healthHistory{db: db}, nil
}

func (store healthHistory) Record(ctx context.Context, records []runtime.ModelHealthRecord) {
	if len(records) == 0 {
		return
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	for _, record := range records {
		_, err := tx.ExecContext(ctx, `INSERT INTO minato_proxy_health
			(id,model_name,model_id,status,healthy_count,unhealthy_count,response_ms,checked_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, record.ID, record.Model, record.ModelID, record.Status,
			record.HealthyCount, record.UnhealthyCount, record.ResponseMS, record.CheckedAt)
		if err != nil {
			return
		}
	}
	tx.Commit()
}

func (store healthHistory) Read(ctx context.Context, query runtime.HealthHistoryQuery) ([]runtime.ModelHealthRecord, error) {
	const columns = `id,model_name,model_id,status,healthy_count,unhealthy_count,response_ms,checked_at`
	const scope = ` WHERE ($1::text[] IS NULL OR model_name = ANY($1::text[])) `
	var rows *sql.Rows
	var err error
	if query.Latest {
		rows, err = store.db.QueryContext(ctx, `SELECT DISTINCT ON (model_id) `+columns+` FROM minato_proxy_health`+scope+
			`ORDER BY model_id, checked_at DESC, id`, pq.Array(query.Scope))
	} else {
		rows, err = store.db.QueryContext(ctx, `SELECT `+columns+` FROM minato_proxy_health`+scope+
			`AND ($2 = '' OR model_name = $2) AND ($3 = '' OR status = $3)
			ORDER BY checked_at DESC, id LIMIT $4 OFFSET $5`, pq.Array(query.Scope), query.Model, query.Status, query.Limit, query.Offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]runtime.ModelHealthRecord, 0)
	for rows.Next() {
		var record runtime.ModelHealthRecord
		if err := rows.Scan(&record.ID, &record.Model, &record.ModelID, &record.Status, &record.HealthyCount,
			&record.UnhealthyCount, &record.ResponseMS, &record.CheckedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = record.CheckedAt
		records = append(records, record)
	}
	return records, rows.Err()
}

func ImportLiteLLMHealth(db *sql.DB) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := OpenHealthHistory(ctx, db); err != nil {
		return 0, err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO minato_proxy_health
		(id,model_name,model_id,status,healthy_count,unhealthy_count,response_ms,checked_at)
		SELECT health_check_id, model_name, COALESCE(NULLIF(model_id,''), model_name), status,
			healthy_count, unhealthy_count, COALESCE(response_time_ms,0), checked_at AT TIME ZONE 'UTC'
		FROM "LiteLLM_HealthCheckTable"
		ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return 0, errors.New("could not import LiteLLM health history")
	}
	return result.RowsAffected()
}