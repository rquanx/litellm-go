package proxyconfig

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

type activityDay struct {
	Date     string  `json:"date"`
	Requests int64   `json:"api_requests"`
	Tokens   int64   `json:"total_tokens"`
	Model    *string `json:"model_group,omitempty"`
}

type activitySeries struct {
	Model    *string       `json:"model,omitempty"`
	Days     []activityDay `json:"daily_data"`
	Requests int64         `json:"sum_api_requests"`
	Tokens   int64         `json:"sum_total_tokens"`
}

func (log *SpendLog) globalActivity(w http.ResponseWriter, r *http.Request) {
	caller, valid := log.keys.identifyReadCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin && caller.user == "" {
		modelError(w, http.StatusForbidden, "A user-bound key is required")
		return
	}
	start, startErr := time.Parse(time.DateOnly, r.URL.Query().Get("start_date"))
	end, endErr := time.Parse(time.DateOnly, r.URL.Query().Get("end_date"))
	if startErr != nil || endErr != nil || end.Before(start) {
		modelJSON(w, http.StatusBadRequest, map[string]any{"detail": map[string]string{
			"error": "Please provide valid start_date and end_date",
		}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	groupByModel := r.URL.Path == "/global/activity/model"
	source := `FROM minato_proxy_spend WHERE started_at >= $1 AND started_at < $2`
	args := []any{start, end.AddDate(0, 0, 1)}
	if !caller.admin {
		source += " AND user_id=$3"
		args = append(args, caller.user)
	}
	query := `SELECT '' AS model, (started_at AT TIME ZONE 'UTC')::date AS day,
		COUNT(*) AS requests, COALESCE(SUM(total_tokens), 0) AS tokens ` + source + ` GROUP BY 1,2 ORDER BY 2`
	if groupByModel {
		query = `WITH daily AS (
			SELECT COALESCE(model_group,'') AS model, (started_at AT TIME ZONE 'UTC')::date AS day,
			COUNT(*) AS requests, COALESCE(SUM(total_tokens),0) AS tokens ` + source + ` GROUP BY 1,2
		), top AS (
			SELECT model, SUM(requests) AS total FROM daily GROUP BY model ORDER BY total DESC, model LIMIT 10
		)
		SELECT daily.model, daily.day, daily.requests, daily.tokens FROM daily JOIN top USING (model)
		ORDER BY top.total DESC, daily.model, daily.day`
	}
	rows, err := log.db.QueryContext(ctx, query, args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read activity")
		return
	}
	defer rows.Close()
	series := []activitySeries{}
	if !groupByModel {
		series = append(series, activitySeries{Days: []activityDay{}})
	}
	for rows.Next() {
		var day activityDay
		var model string
		var date time.Time
		if err := rows.Scan(&model, &date, &day.Requests, &day.Tokens); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read activity")
			return
		}
		if groupByModel {
			day.Model = &model
			if len(series) == 0 || *series[len(series)-1].Model != model {
				series = append(series, activitySeries{Model: &model})
			}
		}
		day.Date = date.Format("Jan 02")
		entry := &series[len(series)-1]
		entry.Days = append(entry.Days, day)
		entry.Requests += day.Requests
		entry.Tokens += day.Tokens
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read activity")
		return
	}
	for _, entry := range series {
		slices.SortStableFunc(entry.Days, func(a, b activityDay) int { return strings.Compare(a.Date, b.Date) })
	}
	if groupByModel {
		modelJSON(w, http.StatusOK, series)
		return
	}
	modelJSON(w, http.StatusOK, series[0])
}
