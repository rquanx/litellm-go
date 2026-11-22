package proxyconfig

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

func (log *SpendLog) ranked(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireMaster(w, r) {
		return
	}
	byKey := r.URL.Path == "/global/spend/keys"
	limit := 10
	if byKey {
		limit = 1000
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			modelError(w, http.StatusUnprocessableEntity, "Invalid spend limit")
			return
		}
		limit = value
	}
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -30)
	stmt := `SELECT model, sum(spend) AS total_spend FROM minato_proxy_spend
		WHERE started_at >= $1 AND model <> '' GROUP BY model ORDER BY total_spend DESC NULLS LAST, model LIMIT $2`
	if byKey {
		stmt = `SELECT api_key, max(NULLIF(key_alias,'')), sum(spend) AS total_spend FROM minato_proxy_spend
			WHERE started_at >= $1 GROUP BY api_key ORDER BY total_spend DESC NULLS LAST, api_key LIMIT $2`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, stmt, from, limit)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not rank spend")
		return
	}
	defer rows.Close()
	results := make([]map[string]any, 0)
	for rows.Next() {
		var id string
		var alias *string
		var spend *float64
		if byKey {
			err = rows.Scan(&id, &alias, &spend)
		} else {
			err = rows.Scan(&id, &spend)
		}
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode spend ranking")
			return
		}
		entry := map[string]any{"total_spend": spend}
		if byKey {
			entry["api_key"], entry["key_alias"], entry["key_name"] = id, alias, nil
		} else {
			entry["model"] = id
		}
		results = append(results, entry)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading spend ranking")
		return
	}
	modelJSON(w, http.StatusOK, results)
}
