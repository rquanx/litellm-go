package proxyconfig

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (log *SpendLog) spendReport(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	query := r.URL.Query()
	start, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
	end, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
	if startErr != nil || endErr != nil || end.Before(start) {
		modelError(w, http.StatusBadRequest, "Please provide valid start_date and end_date")
		return
	}
	group := query.Get("group_by")
	switch group {
	case "":
		group = "team"
	case "team", "customer", "api_key":
	default:
		modelError(w, http.StatusUnprocessableEntity, "Invalid spend report group")
		return
	}
	key, user := query.Get("api_key"), query.Get("internal_user_id")
	team, customer := query.Get("team_id"), query.Get("customer_id")
	from := " FROM minato_proxy_spend WHERE started_at >= $1 AND started_at < $2"
	args := []any{start, end.AddDate(0, 0, 1)}
	var statement string
	switch {
	case key != "":
		if strings.HasPrefix(key, "sk-") {
			key = keyDigest(key)
		}
		statement = spendReportByKey(from + " AND api_key=$3")
		args = append(args, key)
	case user != "":
		statement = spendReportByKey(from + " AND user_id=$3")
		args = append(args, user)
	case team != "" && customer != "":
		statement = spendReportByDay(from+" AND team_id=$3 AND end_user_id=$4", "teams_customers", "team_id", "end_user_id", "$5")
		args = append(args, team, customer, log.reportTeamAliases())
	case group == "api_key":
		statement = spendReportByKey(from)
	case group == "customer":
		statement = spendReportByDay(from, "customers", "''", "end_user_id", "NULL")
	default:
		statement = spendReportByDay(from, "teams", "team_id", "''", "$3")
		args = append(args, log.reportTeamAliases())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, statement, args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend report")
		return
	}
	defer rows.Close()
	result := []json.RawMessage{}
	for rows.Next() {
		var entry []byte
		if err := rows.Scan(&entry); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read spend report")
			return
		}
		result = append(result, entry)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend report")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func (log *SpendLog) reportTeamAliases() string {
	log.keys.mu.RLock()
	aliases := make(map[string]string, len(log.keys.state.Teams))
	for id, team := range log.keys.state.Teams {
		aliases[id] = team.Alias
	}
	log.keys.mu.RUnlock()
	encoded, _ := json.Marshal(aliases)
	return string(encoded)
}

func spendReportByKey(from string) string {
	return `WITH details AS (
		SELECT api_key, model, sum(spend) AS total_cost,
			sum(prompt_tokens) AS total_input_tokens, sum(completion_tokens) AS total_output_tokens` + from + `
		GROUP BY 1,2
	)
	SELECT jsonb_build_object('api_key', api_key, 'total_cost', sum(total_cost),
		'total_input_tokens', sum(total_input_tokens), 'total_output_tokens', sum(total_output_tokens),
		'model_details', jsonb_agg(to_jsonb(details)-'api_key' ORDER BY model))
	FROM details GROUP BY api_key ORDER BY sum(total_cost) DESC NULLS LAST, api_key`
}

func spendReportByDay(from, collection, team, customer, aliases string) string {
	teamField := `jsonb_build_object('team_name', team_name)`
	customerField := `jsonb_build_object('customer', customer)`
	groupFields := teamField
	switch collection {
	case "customers":
		groupFields = customerField
	case "teams_customers":
		groupFields += " || " + customerField
	}
	return `WITH details AS (
		SELECT date_trunc('day', started_at AT TIME ZONE 'UTC') AS day,
			COALESCE(` + aliases + `::jsonb ->> ` + team + `, 'Unassigned Team') AS team_name,
			` + customer + ` AS customer, model, api_key, sum(spend) AS spend, sum(total_tokens) AS total_tokens` + from + `
		GROUP BY 1,2,3,4,5
	), daily AS (
		SELECT day, team_name, customer, ` + groupFields + ` ||
			jsonb_build_object('total_spend', sum(spend),
				'metadata', jsonb_agg(to_jsonb(details)-'day'-'team_name'-'customer' ORDER BY model, api_key)) AS item
		FROM details GROUP BY 1,2,3
	)
	SELECT jsonb_build_object('group_by_day', day, '` + collection + `',
		jsonb_agg(item ORDER BY team_name, customer)) FROM daily GROUP BY day ORDER BY day`
}
