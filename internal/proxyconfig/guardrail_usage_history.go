package proxyconfig

import (
	"context"
	"database/sql"
)

func guardrailRunSource(ctx context.Context, tx *sql.Tx) (string, error) {
	var history bool
	var postgresVersion int
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('"LiteLLM_SpendLogGuardrailIndex"') IS NOT NULL
		AND to_regclass('"LiteLLM_SpendLogs"') IS NOT NULL,current_setting('server_version_num')::int`).Scan(&history, &postgresVersion); err != nil {
		return "", err
	}
	const filters = ` AND ($5='' OR policy_id=$5)
		AND ($3::timestamptz IS NULL OR started_at >= $3)
		AND ($4::timestamptz IS NULL OR started_at <= $4)`
	const names = `($1='' OR guardrail_name=$1 OR guardrail_name=(SELECT guardrail_name FROM ` + guardrailTable + ` WHERE guardrail_id=$1))`
	source := `WITH runs AS (
		SELECT id,started_at,action,model,latency_ms,NULL::double precision AS score,
			NULL::text AS input_snippet,NULL::text AS output_snippet,NULL::text AS reason
		FROM ` + guardrailRunTable + ` WHERE ` + names + filters
	if history {
		metadata := "log.metadata"
		if postgresVersion >= 160000 {
			metadata = `CASE WHEN jsonb_typeof(log.metadata)='string' AND pg_input_is_valid(log.metadata #>> '{}','jsonb')
				THEN (log.metadata #>> '{}')::jsonb ELSE log.metadata END`
		}
		source += `
		UNION ALL
		SELECT log.request_id,idx.start_time AT TIME ZONE 'UTC',
			CASE WHEN outcome ~ 'intervened|block' THEN 'blocked'
				WHEN outcome ~ 'fail|error' THEN 'flagged' ELSE 'passed' END,
			log.model,
			CASE WHEN jsonb_typeof(detail.value->'duration')='number'
				THEN (detail.value->>'duration')::double precision*1000 END,
			CASE WHEN jsonb_typeof(score.value)='number'
				THEN round((score.value #>> '{}')::numeric,2)::double precision END,
			LEFT(NULLIF(COALESCE(NULLIF(log.messages,'{}'::jsonb) #>> '{}',
				log.proxy_server_request->>'messages',log.proxy_server_request->'body'->>'messages'),'{}'),200),
			LEFT(NULLIF(log.response #>> '{}','{}'),200),
			LEFT(detail.value->>'guardrail_response',500)
		FROM "LiteLLM_SpendLogGuardrailIndex" idx
		JOIN "LiteLLM_SpendLogs" log ON log.request_id=idx.request_id
		CROSS JOIN LATERAL (SELECT ` + metadata + ` AS value) meta
		LEFT JOIN LATERAL (
			SELECT candidate.value
			FROM jsonb_array_elements(CASE WHEN jsonb_typeof(meta.value->'guardrail_information')='array'
				THEN meta.value->'guardrail_information' ELSE '[]'::jsonb END) candidate(value)
			WHERE COALESCE(candidate.value->>'guardrail_id',candidate.value->>'guardrail_name')=idx.guardrail_id
			LIMIT 1
		) detail ON true
		CROSS JOIN LATERAL (
			SELECT lower(COALESCE(detail.value->>'guardrail_status','')) AS outcome,
				COALESCE(NULLIF(detail.value->'confidence_score','null'::jsonb),detail.value->'risk_score') AS value
		) score
		WHERE ($1='' OR idx.guardrail_id=$1 OR idx.guardrail_id=(SELECT guardrail_name FROM ` + guardrailTable + ` WHERE guardrail_id=$1))
		AND ($5='' OR idx.policy_id=$5)
		AND ($3::timestamptz IS NULL OR idx.start_time >= ($3 AT TIME ZONE 'UTC'))
		AND ($4::timestamptz IS NULL OR idx.start_time <= ($4 AT TIME ZONE 'UTC'))`
	}
	return source + `) `, nil
}
