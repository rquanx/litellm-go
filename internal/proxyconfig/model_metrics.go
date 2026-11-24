package proxyconfig

import (
	"context"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (log *SpendLog) modelMetrics(w http.ResponseWriter, r *http.Request) {
	caller, valid := log.keys.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin && caller.user == "" {
		modelError(w, http.StatusForbidden, "A user-bound key is required")
		return
	}
	query := r.URL.Query()
	now := time.Now().UTC()
	streaming := r.URL.Path == "/model/streaming_metrics"
	days := 30
	group := query.Get("_selected_model_group")
	if streaming {
		days = 7
	} else if r.URL.Path != "/model/metrics/exceptions" && !query.Has("_selected_model_group") {
		group = "gpt-4-32k"
	}
	start, startErr := spendTime(query.Get("startTime"), now.AddDate(0, 0, -days), false)
	end, endErr := spendTime(query.Get("endTime"), now, false)
	if startErr != nil || endErr != nil || end.Before(start) {
		modelError(w, http.StatusUnprocessableEntity, "Invalid model metrics dates")
		return
	}
	start, end = start.UTC(), end.UTC()
	sameDay := streaming && start.Format(time.DateOnly) == end.Format(time.DateOnly)
	if sameDay {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	}
	args := []any{group, start, end}
	where := ` FROM minato_proxy_spend WHERE model_group=$1 AND started_at >= $2 AND started_at <= $3 AND NOT cache_hit`
	if sameDay {
		where = ` FROM minato_proxy_spend WHERE model_group=$1 AND started_at >= $2 AND started_at < $3 AND NOT cache_hit`
	}
	if !caller.admin {
		where += " AND user_id=$4"
		args = append(args, caller.user)
	}
	if !streaming {
		for name, column := range map[string]string{"api_key": "api_key", "customer": "end_user_id"} {
			value := query.Get(name)
			if value == "" || value == "null" || value == "undefined" {
				continue
			}
			if name == "api_key" && strings.HasPrefix(value, "sk-") {
				value = keyDigest(value)
			}
			args = append(args, value)
			where += " AND " + column + "=$" + strconv.Itoa(len(args))
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if r.URL.Path == "/model/metrics/slow_responses" {
		log.slowResponses(ctx, w, where, args)
		return
	}
	if r.URL.Path == "/model/metrics/exceptions" {
		log.modelExceptions(ctx, w, where, args)
		return
	}
	metric := `AVG(EXTRACT(EPOCH FROM ended_at - started_at) / completion_tokens)`
	if streaming {
		metric = "AVG(first_output_seconds)"
		where += " AND first_output_seconds IS NOT NULL"
	} else {
		where += " AND completion_tokens > 0"
	}
	statement := `SELECT to_char(started_at AT TIME ZONE 'UTC','YYYY-MM-DD'), model, api_origin, ` +
		metric + where + " GROUP BY 1,2,3 ORDER BY 4 DESC, 1, 2, 3"
	if sameDay {
		statement = `SELECT request_id, model, api_origin, first_output_seconds` + where + " ORDER BY 4 DESC, 1"
	}
	rows, err := log.db.QueryContext(ctx, statement, args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read model metrics")
		return
	}
	defer rows.Close()
	entries := map[string]map[string]any{}
	bases := map[string]bool{}
	for rows.Next() {
		var date, model, origin string
		var elapsed float64
		if err := rows.Scan(&date, &model, &origin, &elapsed); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read model metrics")
			return
		}
		name := model
		if strings.HasPrefix(origin, "https://") {
			name, _, _ = strings.Cut(origin, "/openai/")
		}
		if entries[date] == nil {
			entries[date] = map[string]any{"date": date}
		}
		if name != "date" {
			entries[date][name] = elapsed
			bases[name] = true
		}
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read model metrics")
		return
	}
	data := make([]map[string]any, 0, len(entries))
	for _, date := range slices.Sorted(maps.Keys(entries)) {
		data = append(data, entries[date])
	}
	modelJSON(w, http.StatusOK, map[string]any{"data": data, "all_api_bases": slices.Sorted(maps.Keys(bases))})
}

func (log *SpendLog) slowResponses(ctx context.Context, w http.ResponseWriter, where string, args []any) {
	threshold, err := strconv.Atoi(os.Getenv("DEFAULT_SLACK_ALERTING_THRESHOLD"))
	if err != nil || threshold <= 0 {
		threshold = 300
	}
	args = append(args, threshold)
	seconds := "$" + strconv.Itoa(len(args))
	rows, err := log.db.QueryContext(ctx, `SELECT api_origin, COUNT(*),
		COUNT(*) FILTER (WHERE EXTRACT(EPOCH FROM ended_at - started_at) >= `+seconds+`) AS slow_count`+
		where+" GROUP BY 1 ORDER BY slow_count DESC, 1", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read slow responses")
		return
	}
	defer rows.Close()
	type counts struct {
		Origin string `json:"api_base"`
		Total  int64  `json:"total_count"`
		Slow   int64  `json:"slow_count"`
	}
	list := []counts{}
	for rows.Next() {
		var item counts
		if err := rows.Scan(&item.Origin, &item.Total, &item.Slow); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read slow responses")
			return
		}
		list = append(list, item)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read slow responses")
		return
	}
	modelJSON(w, http.StatusOK, list)
}
