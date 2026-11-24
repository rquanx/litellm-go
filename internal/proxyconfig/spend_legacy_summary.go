package proxyconfig

import (
	"context"
	"net/http"
	"time"
)

func (log *SpendLog) legacySpendSummary(w http.ResponseWriter, r *http.Request, where string, args []any) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, `SELECT (started_at AT TIME ZONE 'UTC')::date,
		COALESCE(api_key,''), COALESCE(user_id,''), COALESCE(model,''),
		COALESCE(sum(spend),0), sum(total_tokens), count(*)`+where+" GROUP BY 1,2,3,4 ORDER BY 1", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not summarize spend logs")
		return
	}
	defer rows.Close()

	data := []map[string]any{}
	var current map[string]any
	var day time.Time
	var users, models map[string]float64
	var spend float64
	var tokens, requests int
	for rows.Next() {
		var date time.Time
		var key, user, model string
		var groupSpend float64
		var groupTokens, groupRequests int
		if err := rows.Scan(&date, &key, &user, &model, &groupSpend, &groupTokens, &groupRequests); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode spend summary")
			return
		}
		if !date.Equal(day) || current == nil {
			day = date
			dayString := date.Format(time.DateOnly)
			users, models = map[string]float64{}, map[string]float64{}
			current = map[string]any{"startTime": dayString, "date": dayString, "users": users, "models": models}
			data = append(data, current)
			spend, tokens, requests = 0, 0, 0
		}
		spend += groupSpend
		tokens += groupTokens
		requests += groupRequests
		current["spend"], current["total_tokens"], current["total_requests"] = spend, tokens, requests
		users[user] += groupSpend
		models[model] += groupSpend
		if len(key) == 64 {
			keySpend, _ := current[key].(float64)
			current[key] = keySpend + groupSpend
		}
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading spend summary")
		return
	}
	modelJSON(w, http.StatusOK, data)
}

