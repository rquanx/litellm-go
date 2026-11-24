package proxyconfig

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

type customerMetrics struct {
	Spend      float64 `json:"spend"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Total      int64   `json:"total_tokens"`
	Requests   int64   `json:"api_requests"`
	Success    int64   `json:"successful_requests"`
	Failure    int64   `json:"failed_requests"`
	CacheRead  int64   `json:"cache_read_input_tokens"`
	CacheWrite int64   `json:"cache_creation_input_tokens"`
}

func (total *customerMetrics) add(entry customerMetrics) {
	total.Spend += entry.Spend
	total.Prompt += entry.Prompt
	total.Completion += entry.Completion
	total.Total += entry.Total
	total.Requests += entry.Requests
	total.Success += entry.Success
	total.Failure += entry.Failure
	total.CacheRead += entry.CacheRead
	total.CacheWrite += entry.CacheWrite
}

type customerBreakdown struct {
	Metrics  customerMetrics              `json:"metrics"`
	Metadata map[string]any               `json:"metadata"`
	Keys     map[string]customerBreakdown `json:"api_key_breakdown,omitempty"`
}

type customerActivityDay struct {
	Date      string                                  `json:"date"`
	Metrics   customerMetrics                         `json:"metrics"`
	Breakdown map[string]map[string]customerBreakdown `json:"breakdown"`
}

type activityScope struct {
	ids     []string
	aliases map[string]string
	user    string
}

func (log *SpendLog) customerActivity(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	log.entityActivity(w, r, "end_user", activityScope{})
}

func (log *SpendLog) organizationActivity(w http.ResponseWriter, r *http.Request) {
	aliases, scope, ok := log.organizationActivityScope(w, r)
	if ok {
		log.entityActivity(w, r, "organization", activityScope{ids: scope, aliases: aliases})
	}
}

func (log *SpendLog) organizationActivityScope(w http.ResponseWriter, r *http.Request) (map[string]string, []string, bool) {
	log.keys.mu.RLock()
	defer log.keys.mu.RUnlock()
	caller, ok := log.keys.identifyReadCallerLocked(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return nil, nil, false
	}
	aliases := make(map[string]string)
	var scope []string
	if !caller.admin {
		scope = []string{}
	}
	for id, org := range log.keys.state.Organizations {
		if caller.admin || organizationRole(org, caller.user) == "org_admin" {
			aliases[id] = org.Alias
			if !caller.admin {
				scope = append(scope, id)
			}
		}
	}
	if ids := r.URL.Query().Get("organization_ids"); ids != "" && scope != nil {
		for _, id := range strings.Split(ids, ",") {
			if _, allowed := aliases[id]; !allowed {
				modelError(w, http.StatusForbidden, "You do not have access to this organization")
				return nil, nil, false
			}
		}
	}
	return aliases, scope, true
}

func (log *SpendLog) agentActivity(w http.ResponseWriter, r *http.Request) {
	token := runtime.ProxyCredential(r)
	if token == log.keys.master && token != "" {
		log.entityActivity(w, r, "agent", activityScope{})
		return
	}
	if _, ok := log.keys.Authorize(token); !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	log.entityActivity(w, r, "agent", activityScope{ids: log.agentActivityScope(r)})
}

func (log *SpendLog) tagActivity(w http.ResponseWriter, r *http.Request) {
	caller, valid := log.keys.identifyResourceCaller(r)
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if caller.admin {
		log.entityActivity(w, r, "tag", activityScope{})
		return
	}
	if caller.user == "" {
		modelError(w, http.StatusForbidden, "A user-bound key is required")
		return
	}
	log.entityActivity(w, r, "tag", activityScope{user: caller.user})
}

func (log *SpendLog) entityActivity(w http.ResponseWriter, r *http.Request, entity string, scope activityScope) {
	query := r.URL.Query()
	aggregate := r.URL.Path == "/user/daily/activity/aggregated"
	defaultSize, maxSize := 10, 100
	if entity == "user" {
		defaultSize, maxSize = 50, 1000
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", defaultSize)
	if aggregate {
		page, size, pageErr, sizeErr = 1, defaultSize, nil, nil
	}
	start, startErr := time.Parse(time.DateOnly, query.Get("start_date"))
	end, endErr := time.Parse(time.DateOnly, query.Get("end_date"))
	if entity == "tag" {
		if query.Get("end_date") == "" {
			end, endErr = time.Parse(time.DateOnly, time.Now().UTC().Format(time.DateOnly))
		}
		if query.Get("start_date") == "" {
			start, startErr = end.AddDate(0, 0, -30), nil
		}
	}
	end = end.AddDate(0, 0, 1)
	if pageErr != nil || sizeErr != nil || startErr != nil || endErr != nil || !start.Before(end) || size > maxSize ||
		page-1 > int(^uint(0)>>1)/size {
		modelError(w, http.StatusBadRequest, "Invalid customer activity filters or pagination")
		return
	}
	day := "(started_at AT TIME ZONE 'UTC')"
	if raw := query.Get("timezone"); entity == "user" && raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < -1440 || offset > 1440 {
			modelError(w, http.StatusBadRequest, "Invalid timezone offset")
			return
		}
		start = start.Add(time.Duration(offset) * time.Minute)
		end = end.Add(time.Duration(offset) * time.Minute)
		day = "(" + day + " - INTERVAL '1 minute' * " + strconv.Itoa(offset) + ")"
	}
	where, args, err := spendFilter(query, start, end)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	column, aliasJoin, aliasField := "end_user_id", " LEFT JOIN "+customerTable+" customer ON customer.user_id=activity.entity_id", "customer.alias"
	if entity == "agent" {
		column, aliasJoin, aliasField = "COALESCE(NULLIF(agent_id,''),CASE WHEN provider='a2a_agent' THEN model_id ELSE '' END)", "", "NULL::text"
	}
	if entity == "organization" || entity == "team" || entity == "user" {
		column, aliasJoin, aliasField = entity+"_id", "", "NULL::text"
	}
	if entity == "tag" {
		column, aliasJoin, aliasField = "tag_name", "", "NULL::text"
		where = strings.Replace(where, "FROM minato_proxy_spend WHERE",
			"FROM minato_proxy_spend CROSS JOIN LATERAL unnest(tags) AS tag(tag_name) WHERE", 1)
		if tags := query.Get("tags"); tags != "" {
			args = append(args, pq.Array(strings.Split(tags, ",")))
			where += " AND tag_name=ANY($" + strconv.Itoa(len(args)) + ")"
		}
	}
	if scope.user != "" {
		args = append(args, scope.user)
		where += " AND user_id=$" + strconv.Itoa(len(args))
	}
	where += " AND " + column + " <> ''"
	if scope.ids != nil {
		args = append(args, pq.Array(scope.ids))
		where += " AND " + column + " = ANY($" + strconv.Itoa(len(args)) + ")"
	}
	if ids := query.Get(entity + "_ids"); ids != "" {
		args = append(args, pq.Array(strings.Split(ids, ",")))
		where += " AND " + column + " = ANY($" + strconv.Itoa(len(args)) + ")"
	}
	if ids := query.Get("exclude_" + entity + "_ids"); ids != "" {
		args = append(args, pq.Array(strings.Split(ids, ",")))
		where += " AND NOT (" + column + " = ANY($" + strconv.Itoa(len(args)) + "))"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := log.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not query customer activity")
		return
	}
	defer tx.Rollback()
	var count int
	columns := day + `::date AS day, ` + column + ` AS entity_id, model, model_group, provider,
		call_type, api_key, key_alias, team_id, COALESCE(sum(spend),0) AS spend, sum(prompt_tokens) AS prompt,
		sum(completion_tokens) AS completion, sum(total_tokens) AS tokens, count(*) AS requests,
		count(*) FILTER (WHERE status = 'success') AS successes, count(*) FILTER (WHERE status = 'failure') AS failures,
		sum(cache_read_input_tokens) AS cache_read, sum(cache_creation_input_tokens) AS cache_write`
	filtered := "SELECT " + columns + where + " GROUP BY 1,2,3,4,5,6,7,8,9"
	order := " ORDER BY day DESC, entity_id, model, model_group, provider, call_type, api_key, key_alias, team_id"
	if !aggregate {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ("+filtered+") activity", args...).Scan(&count); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not count customer activity")
			return
		}
		order += " LIMIT $" + strconv.Itoa(len(args)+1) + " OFFSET $" + strconv.Itoa(len(args)+2)
		args = append(args, size, (page-1)*size)
	}
	rows, err := tx.QueryContext(ctx, "SELECT activity.*, "+aliasField+" FROM ("+filtered+order+
		") activity"+aliasJoin+" ORDER BY activity.day DESC", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not aggregate customer activity")
		return
	}
	defer rows.Close()
	result := []customerActivityDay{}
	var totals customerMetrics
	for rows.Next() {
		var day time.Time
		var id, model, group, provider, call, key, keyAlias, team string
		var alias *string
		var metrics customerMetrics
		if err := rows.Scan(&day, &id, &model, &group, &provider, &call, &key, &keyAlias, &team, &metrics.Spend,
			&metrics.Prompt, &metrics.Completion, &metrics.Total, &metrics.Requests, &metrics.Success, &metrics.Failure,
			&metrics.CacheRead, &metrics.CacheWrite, &alias); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode customer activity")
			return
		}
		date := day.Format(time.DateOnly)
		if len(result) == 0 || result[len(result)-1].Date != date {
			entry := customerActivityDay{Date: date, Breakdown: make(map[string]map[string]customerBreakdown)}
			for _, dimension := range []string{"mcp_servers", "models", "model_groups", "providers", "endpoints", "api_keys", "entities"} {
				entry.Breakdown[dimension] = make(map[string]customerBreakdown)
			}
			result = append(result, entry)
		}
		entry := &result[len(result)-1]
		entry.Metrics.add(metrics)
		totals.add(metrics)
		keyMetadata := map[string]any{"key_alias": keyAlias, "team_id": team}
		for dimension, value := range map[string]string{"models": model, "model_groups": group, "providers": provider, "endpoints": call, "api_keys": key, "entities": id} {
			if value == "" {
				continue
			}
			details := entry.Breakdown[dimension][value]
			if details.Metadata == nil {
				details.Metadata = make(map[string]any)
			}
			if dimension == "entities" {
				if entityAlias, ok := scope.aliases[id]; ok {
					details.Metadata[entity+"_alias"] = entityAlias
				} else if alias != nil || entity == "end_user" || entity == "agent" || entity == "tag" {
					details.Metadata["alias"] = alias
				}
			}
			if dimension == "api_keys" {
				details.Metadata = keyMetadata
			} else if key != "" {
				if details.Keys == nil {
					details.Keys = make(map[string]customerBreakdown)
				}
				keyDetails := details.Keys[key]
				keyDetails.Metadata = keyMetadata
				keyDetails.Metrics.add(metrics)
				details.Keys[key] = keyDetails
			}
			details.Metrics.add(metrics)
			entry.Breakdown[dimension][value] = details
		}
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not read customer activity")
		return
	}
	totalPages := (count + size - 1) / size
	if aggregate {
		totalPages = 1
	}
	modelJSON(w, http.StatusOK, map[string]any{"results": result, "metadata": map[string]any{
		"total_spend": totals.Spend, "total_prompt_tokens": totals.Prompt, "total_completion_tokens": totals.Completion, "total_tokens": totals.Total,
		"total_api_requests": totals.Requests, "total_successful_requests": totals.Success, "total_failed_requests": totals.Failure,
		"total_cache_read_input_tokens": totals.CacheRead, "total_cache_creation_input_tokens": totals.CacheWrite,
		"page": page, "total_pages": totalPages, "has_more": page < totalPages,
	}})
}
