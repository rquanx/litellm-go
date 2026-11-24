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
	var hasCacheHit, hasEndUser, hasAgentID, hasAPIBase, hasFirstOutput, hasOrgID, hasTags, hasSessionID bool
	if err := db.QueryRowContext(ctx, `SELECT
		COALESCE(bool_or(attname='cache_hit'),false), COALESCE(bool_or(attname='end_user'),false), COALESCE(bool_or(attname='agent_id'),false),
		COALESCE(bool_or(attname='api_base'),false), COALESCE(bool_or(attname='completionStartTime'),false),
		COALESCE(bool_or(attname='organization_id'),false), COALESCE(bool_or(attname='request_tags'),false), COALESCE(bool_or(attname='session_id'),false)
		FROM pg_attribute WHERE attrelid=to_regclass('"LiteLLM_SpendLogs"') AND NOT attisdropped`).
		Scan(&hasCacheHit, &hasEndUser, &hasAgentID, &hasAPIBase, &hasFirstOutput, &hasOrgID, &hasTags, &hasSessionID); err != nil {
		return 0, errors.New("could not inspect LiteLLM spend records")
	}
	cacheHit := "false"
	if hasCacheHit {
		cacheHit = "COALESCE(lower(cache_hit::text) = 'true', false)"
	}
	endUser := "''"
	if hasEndUser {
		endUser = "COALESCE(end_user,'')"
	}
	agentID := "COALESCE(metadata->>'agent_id','')"
	if hasAgentID {
		agentID = "COALESCE(NULLIF(agent_id,''),metadata->>'agent_id','')"
	}
	apiOrigin := "''"
	if hasAPIBase {
		apiOrigin = `COALESCE(substring(api_base FROM '^(https?://[^/?#@[:space:]]+)(?:[/?#]|$)'), '')`
	}
	firstOutput := "NULL"
	if hasFirstOutput {
		firstOutput = `CASE WHEN "completionStartTime" >= "startTime" AND "completionStartTime" < "endTime"
			THEN EXTRACT(EPOCH FROM ("completionStartTime" - "startTime")) END`
	}
	orgID := "''"
	if hasOrgID {
		orgID = "COALESCE(organization_id,'')"
	}
	tags := `'{}'::text[]`
	if hasTags {
		tags = `ARRAY(SELECT DISTINCT item #>> '{}' FROM jsonb_array_elements(
			CASE WHEN jsonb_typeof(request_tags::jsonb)='array' THEN request_tags::jsonb
				WHEN jsonb_typeof(request_tags::jsonb)='string' AND (request_tags::jsonb #>> '{}') IS JSON ARRAY
				THEN (request_tags::jsonb #>> '{}')::jsonb ELSE '[]'::jsonb END
			) AS elem(item) WHERE jsonb_typeof(item)='string' AND item #>> '{}' <> '' ORDER BY 1)`
	}
	sessionID := "''"
	if hasSessionID {
		sessionID = "CASE WHEN octet_length(session_id) <= 256 THEN COALESCE(session_id,'') ELSE '' END"
	}
	result, err := db.ExecContext(ctx, `INSERT INTO minato_proxy_spend
		(request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,
		provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit,end_user_id,agent_id,api_origin,first_output_seconds,organization_id,tags,session_id)
		SELECT request_id, "startTime" AT TIME ZONE 'UTC', "endTime" AT TIME ZONE 'UTC',
			CASE WHEN api_key ~ '^[0-9a-fA-F]{64}$' THEN lower(api_key) ELSE '' END,
			COALESCE(metadata->>'user_api_key_alias',''),
			COALESCE("user",''), COALESCE(team_id,''),
			COALESCE(metadata->>'project_id',metadata->>'user_api_key_project_id',''),
			COALESCE(model,''),COALESCE(model_group,''),COALESCE(model_id,''),
			COALESCE(custom_llm_provider,''),call_type,COALESCE(status,''),
			prompt_tokens,completion_tokens,total_tokens,spend,
			`+cacheHit+`,`+endUser+`,`+agentID+`,`+apiOrigin+`,`+firstOutput+`,`+orgID+`,`+tags+`,`+sessionID+`
		FROM "LiteLLM_SpendLogs"
		ON CONFLICT (request_id) DO NOTHING`)
	if err != nil {
		return 0, errors.New("could not import LiteLLM spend records")
	}
	return result.RowsAffected()
}
