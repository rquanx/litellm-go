package proxyconfig

import (
	"context"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
)

const spendTagRows = ` FROM minato_proxy_spend CROSS JOIN LATERAL unnest(tags) AS tag(name)`

func (log *SpendLog) spendTagNames(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, "SELECT DISTINCT name"+spendTagRows+" ORDER BY name")
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend tag names")
		return
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read spend tag names")
			return
		}
		names = append(names, name)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend tag names")
		return
	}
	modelJSON(w, http.StatusOK, names)
}

func (log *SpendLog) spendByTag(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	global := r.URL.Path == "/global/spend/tags"
	var filters string
	var args []any
	if global {
		query := r.URL.Query()
		start, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
		end, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
		if startErr != nil || endErr != nil || end.Before(start) {
			modelError(w, http.StatusBadRequest, "Please provide valid start_date and end_date")
			return
		}
		filters = " WHERE started_at >= $1 AND started_at < $2"
		args = append(args, start, end.AddDate(0, 0, 1))
		if raw := query.Get("tags"); raw != "" {
			names := strings.Split(raw, ",")
			if !slices.Contains(names, "all-tags") {
				filters += " AND tags && $3::text[] AND name=ANY($3::text[])"
				args = append(args, pq.Array(names))
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, "SELECT name, count(*), sum(spend)"+spendTagRows+filters+
		" GROUP BY name ORDER BY sum(spend) DESC NULLS LAST, name", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend by tag")
		return
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var name string
		var count int64
		var spend *float64
		if err := rows.Scan(&name, &count, &spend); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read spend by tag")
			return
		}
		if global {
			total := 0.0
			if spend != nil {
				total = math.RoundToEven(*spend*1e4) / 1e4
			}
			result = append(result, map[string]any{"name": name, "spend": total, "log_count": count})
		} else {
			result = append(result, map[string]any{"individual_request_tag": name, "total_spend": spend, "log_count": count})
		}
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend by tag")
		return
	}
	if global {
		modelJSON(w, http.StatusOK, map[string]any{"spend_per_tag": result})
		return
	}
	modelJSON(w, http.StatusOK, result)
}
