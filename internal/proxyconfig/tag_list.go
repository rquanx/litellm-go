package proxyconfig

import (
	"context"
	"errors"
	"net/url"
	"time"
)

func (registry tagRegistry) list(ctx context.Context, query url.Values, user string) ([]storedTag, error) {
	var start, end *time.Time
	if query.Get("start_date") != "" || query.Get("end_date") != "" {
		from, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
		to, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
		if startErr != nil || endErr != nil || to.Before(from) {
			return nil, customerInputError{errors.New("start_date and end_date must specify a valid date range")}
		}
		inclusive := to.AddDate(0, 0, 1)
		start, end = &from, &inclusive
	}
	rows, err := registry.db.QueryContext(ctx, `WITH observed AS (
		SELECT name, MIN(started_at) AS created_at, MAX(ended_at) AS updated_at
		FROM minato_proxy_spend CROSS JOIN LATERAL unnest(tags) AS used(name)
		WHERE ($1::timestamptz IS NULL OR started_at >= $1)
			AND ($2::timestamptz IS NULL OR started_at < $2)
			AND ($3 = '' OR user_id=$3) GROUP BY name
	)
	SELECT t.tag_name,t.description,t.models,t.model_info,t.created_at,t.updated_at,t.created_by,
		(SELECT to_jsonb(b)-'budget_period_started_at' FROM `+budgetTable+` b WHERE b.budget_id=t.budget_id)
	FROM `+tagTable+` t WHERE $3 = '' OR EXISTS (SELECT 1 FROM observed WHERE observed.name=t.tag_name)
	UNION ALL
	SELECT name,'This is just a spend tag that was passed dynamically in a request. It does not control any LLM models.',
		NULL,NULL,created_at,updated_at,NULL,NULL
	FROM observed WHERE NOT EXISTS (SELECT 1 FROM `+tagTable+` t WHERE t.tag_name=observed.name)
	ORDER BY 1`, start, end, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []storedTag{}
	for rows.Next() {
		tag, err := scanTag(rows, true)
		if err != nil {
			return nil, err
		}
		tags = append(tags, tag)
	}
	return tags, rows.Err()
}
