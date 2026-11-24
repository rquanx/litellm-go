package proxyconfig

import (
	"context"
	"net/http"
	"time"
)

func (log *SpendLog) cacheActivity(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	query := r.URL.Query()
	start, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
	end, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
	if startErr != nil || endErr != nil || end.Before(start) {
		modelJSON(w, http.StatusBadRequest, map[string]any{"detail": map[string]string{"error": "Provide valid start_date and end_date"}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(key_alias,''),'Unnamed Key'),call_type,model,
		count(*),count(*) FILTER (WHERE cache_hit),
		COALESCE(sum(completion_tokens) FILTER (WHERE cache_hit),0),
		COALESCE(sum(completion_tokens) FILTER (WHERE NOT cache_hit),0)
		FROM minato_proxy_spend WHERE started_at >= $1 AND started_at < $2
		GROUP BY 1,2,3 ORDER BY 1,2,3`, start, end.AddDate(0, 0, 1))
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not aggregate cache activity")
		return
	}
	defer rows.Close()
	type activity struct {
		Key       string `json:"api_key"`
		Call      string `json:"call_type"`
		Model     string `json:"model"`
		Total     int64  `json:"total_rows"`
		Hits      int64  `json:"cache_hit_true_rows"`
		Cached    int64  `json:"cached_completion_tokens"`
		Generated int64  `json:"generated_completion_tokens"`
	}
	data := make([]activity, 0)
	for rows.Next() {
		var entry activity
		if err := rows.Scan(&entry.Key, &entry.Call, &entry.Model, &entry.Total, &entry.Hits, &entry.Cached, &entry.Generated); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode cache activity")
			return
		}
		data = append(data, entry)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read cache activity")
		return
	}
	modelJSON(w, http.StatusOK, data)
}
