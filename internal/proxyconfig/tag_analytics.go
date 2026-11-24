package proxyconfig

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lib/pq"
)

const tagAnalyticsWindow = 7

func (log *SpendLog) registerTagAnalytics(mux *http.ServeMux) {
	mux.HandleFunc("GET /tag/distinct", log.tagAnalytics)
	mux.HandleFunc("GET /tag/dau", log.tagAnalytics)
	mux.HandleFunc("GET /tag/wau", log.tagAnalytics)
	mux.HandleFunc("GET /tag/mau", log.tagAnalytics)
	mux.HandleFunc("GET /tag/summary", log.tagAnalytics)
	mux.HandleFunc("GET /tag/user-agent/per-user-analytics", log.tagPerUser)
	mux.HandleFunc("GET /in_product_nudges", log.productNudges)
}

func (log *SpendLog) tagAnalytics(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	path := r.URL.Path
	days, period := 1, ""
	switch path {
	case "/tag/wau":
		days, period = 7, "Week"
	case "/tag/mau":
		days, period = 30, "Month"
	}
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	start := tomorrow.AddDate(0, 0, -(tagAnalyticsWindow*days - 1))
	end := tomorrow.AddDate(0, 0, 1)
	if path == "/tag/summary" {
		var startErr, endErr error
		start, startErr = time.Parse(time.DateOnly, r.URL.Query().Get("start_date"))
		end, endErr = time.Parse(time.DateOnly, r.URL.Query().Get("end_date"))
		if startErr != nil || endErr != nil || end.Before(start) {
			modelError(w, http.StatusBadRequest, "Provide valid start_date and end_date")
			return
		}
		end = end.AddDate(0, 0, 1)
	}
	where, args := tagAnalyticsFilter(r.URL.Query(), start, end)

	var rows *sql.Rows
	var err error
	switch path {
	case "/tag/distinct":
		rows, err = log.db.QueryContext(ctx, "SELECT name"+spendTagRows+
			" WHERE name LIKE 'User-Agent:%' OR name NOT LIKE '%:%' GROUP BY name ORDER BY count(*) DESC, name LIMIT 250")
	case "/tag/summary":
		rows, err = log.db.QueryContext(ctx, `SELECT name, count(DISTINCT NULLIF(user_id,'')), count(*),
			count(*) FILTER(WHERE status='success'), count(*) FILTER(WHERE status='failure'),
			COALESCE(sum(total_tokens),0), COALESCE(sum(spend),0)`+spendTagRows+where+
			" GROUP BY name ORDER BY count(*) DESC, name", args...)
	default:
		args = append(args, days)
		index := fmt.Sprint(len(args))
		rows, err = log.db.QueryContext(ctx, `SELECT name, (($2::timestamptz AT TIME ZONE 'UTC')::date - 1
			- (started_at AT TIME ZONE 'UTC')::date)/$`+index+`::integer, count(DISTINCT user_id)`+
			spendTagRows+where+" AND user_id<>'' GROUP BY 1,2 ORDER BY 2 DESC, 3 DESC, 1", args...)
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read tag analytics")
		return
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var tag string
		var row map[string]any
		switch path {
		case "/tag/distinct":
			err = rows.Scan(&tag)
			row = map[string]any{"tag": tag}
		case "/tag/summary":
			var users, requests, success, failed, tokens int64
			var spend float64
			err = rows.Scan(&tag, &users, &requests, &success, &failed, &tokens, &spend)
			row = map[string]any{"tag": tag, "unique_users": users, "total_requests": requests,
				"successful_requests": success, "failed_requests": failed, "total_tokens": tokens, "total_spend": spend}
		default:
			var offset, users int
			err = rows.Scan(&tag, &offset, &users)
			last := tomorrow.AddDate(0, 0, -offset*days)
			row = map[string]any{"tag": tag, "active_users": users, "date": last.Format(time.DateOnly)}
			if period != "" {
				first := last.AddDate(0, 0, 1-days)
				format := "Jan 02"
				if days == 30 {
					format = "Jan"
				}
				row["date"] = fmt.Sprintf("%s %d (%s)", period, tagAnalyticsWindow-offset, first.Format(format))
				row["period_start"] = first.Format(time.DateOnly)
				row["period_end"] = last.Format(time.DateOnly)
			}
		}
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode tag analytics")
			return
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading tag analytics")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"results": result})
}


func tagAnalyticsFilter(query url.Values, start, end time.Time) (string, []any) {
	where := " WHERE started_at >= $1 AND started_at < $2"
	args := []any{start, end}
	if tags := query["tag_filters"]; len(tags) > 0 {
		args = append(args, pq.Array(tags))
		where += " AND tags && $3::text[] AND name=ANY($3::text[])"
	} else if tag := query.Get("tag_filter"); tag != "" {
		args = append(args, tag)
		where += " AND strpos(lower(name), lower($3)) > 0"
	}

	return where, args
}

func (log *SpendLog) tagPerUser(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	query := r.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 50)
	if pageErr != nil || sizeErr != nil || size > 1000 || page-1 > int(^uint(0)>>1)/size {
		modelError(w, http.StatusBadRequest, "Invalid user analytics pagination")
		return
	}
	tomorrow := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	where, args := tagAnalyticsFilter(query, tomorrow.AddDate(0, 0, -30), tomorrow.AddDate(0, 0, 1))
	where += " AND user_id<>''"
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := log.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read per-user analytics")
		return
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(DISTINCT user_id)"+spendTagRows+where, args...).Scan(&count); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not count per-user analytics")
		return
	}
	limit, offset := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := tx.QueryContext(ctx, `SELECT user_id, min(name), count(*),
		count(*) FILTER(WHERE status='success'), count(*) FILTER(WHERE status='failure'),
		COALESCE(sum(total_tokens),0), COALESCE(sum(spend),0)`+
		spendTagRows+where+" GROUP BY user_id ORDER BY 4 DESC, user_id LIMIT $"+limit+" OFFSET $"+offset,
		append(args, size, (page-1)*size)...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read per-user analytics")
		return
	}
	defer rows.Close()
	type userActivity struct {
		ID string `json:"user_id"`
		Email *string `json:"user_email"`
		Tag string `json:"user_agent"`
		Requests int64 `json:"total_requests"`
		Success int64 `json:"successful_requests"`
		Failures int64 `json:"failed_requests"`
		Tokens int64 `json:"total_tokens"`
		Spend float64 `json:"spend"`
	}
	result := []userActivity{}
	for rows.Next() {
		var entry userActivity
		if err := rows.Scan(&entry.ID, &entry.Tag, &entry.Requests, &entry.Success, &entry.Failures, &entry.Tokens, &entry.Spend); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode per-user analytics")
			return
		}
		result = append(result, entry)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading per-user analytics")
		return
	}
	if err := tx.Commit(); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading per-user analytics")
		return
	}
	log.keys.mu.RLock()
	for i := range result {
		if email := log.keys.state.Users[result[i].ID].Email; email != "" {
			result[i].Email = &email
		}
	}
	log.keys.mu.RUnlock()
	modelJSON(w, http.StatusOK, map[string]any{"results": result, "total_count": count,
		"page": page, "page_size": size, "total_pages": (count+size-1)/size})
}

func (log *SpendLog) productNudges(w http.ResponseWriter, r *http.Request) {
	if _, valid := log.keys.identifyReadCaller(r); !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var available bool
	if err := log.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM minato_proxy_spend
		WHERE cardinality(tags)>0 AND tags @> ARRAY['User-Agent: claude-cli'])`).Scan(&available); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read product usage")
		return
	}
	modelJSON(w, http.StatusOK, map[string]bool{"is_claude_code_enabled": available})
}
