package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ImportLiteLLMSpend copies accounting metadata using request IDs for idempotency.
func ImportLiteLLMSpend(db *sql.DB) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := OpenSpendLog(ctx, db, nil); err != nil {
		return 0, err
	}
	var hasCacheHit bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
		WHERE attrelid = to_regclass('"LiteLLM_SpendLogs"') AND attname = 'cache_hit' AND NOT attisdropped)`).Scan(&hasCacheHit); err != nil {
		return 0, errors.New("could not inspect LiteLLM spend records")
	}
	cacheHit := "false"
	if hasCacheHit {
		cacheHit = "COALESCE(lower(cache_hit::text) = 'true', false)"
	}
	result, err := db.ExecContext(ctx, `INSERT INTO minato_proxy_spend
		(request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,
		provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit)
		SELECT request_id, "startTime" AT TIME ZONE 'UTC', "endTime" AT TIME ZONE 'UTC',
			CASE WHEN api_key ~ '^[0-9a-fA-F]{64}$' THEN lower(api_key) ELSE '' END,
			COALESCE(metadata->>'user_api_key_alias',''),
			COALESCE("user",''), COALESCE(team_id,''),
			COALESCE(metadata->>'project_id',metadata->>'user_api_key_project_id',''),
			COALESCE(model,''),COALESCE(model_group,''),COALESCE(model_id,''),
			COALESCE(custom_llm_provider,''),call_type,COALESCE(status,''),
			prompt_tokens,completion_tokens,total_tokens,spend,
			`+cacheHit+`
		FROM "LiteLLM_SpendLogs"
		ON CONFLICT (request_id) DO NOTHING`)
	if err != nil {
		return 0, errors.New("could not import LiteLLM spend records")
	}
	return result.RowsAffected()
}
