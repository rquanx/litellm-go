package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

type SpendLog struct {
	providerBudgets map[string]providerBudget
	storePayloads bool
	maxBudget float64
	globalBudgetDuration string
	globalBudgetAnchor time.Time
	disableGlobalBudgetReset bool
	globalSpendResetAt atomic.Int64
	db   *sql.DB
	keys *KeyRegistry
	agentActivityScope func(*http.Request) []string
}

func OpenSpendLog(ctx context.Context, db *sql.DB, keys *KeyRegistry) (*SpendLog, error) {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS minato_proxy_spend (
		request_id TEXT PRIMARY KEY,
		started_at TIMESTAMPTZ NOT NULL,
		ended_at TIMESTAMPTZ NOT NULL,
		api_key TEXT NOT NULL,
		key_alias TEXT NOT NULL,
		user_id TEXT NOT NULL,
		team_id TEXT NOT NULL,
		project_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL,
		model_group TEXT NOT NULL,
		model_id TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL,
		call_type TEXT NOT NULL,
		status TEXT NOT NULL,
		prompt_tokens BIGINT NOT NULL,
		completion_tokens BIGINT NOT NULL,
		total_tokens BIGINT NOT NULL,
		spend DOUBLE PRECISION
	);
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS model_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS project_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS cache_hit BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS end_user_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS cache_read_input_tokens BIGINT NOT NULL DEFAULT 0;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS cache_creation_input_tokens BIGINT NOT NULL DEFAULT 0;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS key_spend_identity TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS api_origin TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS first_output_seconds DOUBLE PRECISION;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS http_status INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS organization_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_session ON minato_proxy_spend (session_id, started_at) WHERE session_id<>'';
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS input_messages JSONB;
	ALTER TABLE minato_proxy_spend ADD COLUMN IF NOT EXISTS response_payload JSONB;
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_tags ON minato_proxy_spend USING GIN(tags) WHERE cardinality(tags)>0;
	CREATE TABLE IF NOT EXISTS minato_proxy_key_model_spend (
		key_identity TEXT NOT NULL,
		model_group TEXT NOT NULL,
		duration TEXT NOT NULL,
		reset_at TIMESTAMPTZ NOT NULL,
		spend DOUBLE PRECISION NOT NULL,
		PRIMARY KEY (key_identity, model_group, duration)
	);
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_org_started ON minato_proxy_spend (organization_id, started_at)
		WHERE organization_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_stream_latency ON minato_proxy_spend (model_group,started_at)
		WHERE first_output_seconds IS NOT NULL AND NOT cache_hit;
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_key_budget ON minato_proxy_spend
		((COALESCE(NULLIF(key_spend_identity,''),api_key)), started_at) WHERE spend IS NOT NULL;
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_team_member ON minato_proxy_spend (team_id, user_id);
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_user_started ON minato_proxy_spend (user_id, started_at)
		WHERE user_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_team_started ON minato_proxy_spend (team_id, started_at)
		WHERE team_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_project_started ON minato_proxy_spend (project_id, started_at)
		WHERE project_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_agent ON minato_proxy_spend (agent_id, started_at) WHERE agent_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_end_user ON minato_proxy_spend (end_user_id, started_at) WHERE end_user_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_started_at ON minato_proxy_spend (started_at DESC, request_id);`)
	if err != nil {
		return nil, errors.New("could not initialize Go proxy spend logs")
	}
	if err := openTagTable(ctx, db); err != nil {
		return nil, err
	}
	store := &SpendLog{db: db, keys: keys}
	if keys != nil {
		if err := openCustomerTable(ctx, db); err != nil {
			return nil, err
		}
		if err := openBudgetTable(ctx, db); err != nil {
			return nil, err
		}
		keys.keySpend = store.KeySpend
		keys.userSpend = store.UserSpend
		keys.teamSpend = store.TeamSpend
		keys.budgetMaximum = store.budgetMaximum
		keys.budgetPeriod = store.budgetPeriod
	}
	return store, nil
}

type spendRow struct {
	ID         string    `json:"request_id"`
	Started    time.Time `json:"startTime"`
	Ended      time.Time `json:"endTime"`
	Key        string    `json:"api_key"`
	KeyAlias   string    `json:"key_alias"`
	User       string    `json:"user"`
	EndUser    string    `json:"end_user,omitempty"`
	Team       string    `json:"team_id"`
	Project    string    `json:"project_id"`
	Model      string    `json:"model"`
	Group      string    `json:"model_group"`
	ModelID    string    `json:"model_id"`
	Provider   string    `json:"custom_llm_provider"`
	Call       string    `json:"call_type"`
	Status     string    `json:"status"`
	Prompt     int       `json:"prompt_tokens"`
	Completion int       `json:"completion_tokens"`
	Total      int       `json:"total_tokens"`
	Spend      *float64  `json:"spend"`
	CacheHit   bool      `json:"cache_hit"`
	Organization string `json:"organization_id"`
	Agent string `json:"agent_id"`
	APIBase string `json:"api_base"`
	FirstOutputSeconds *float64 `json:"-"`
	CompletionStarted *time.Time `json:"completionStartTime"`
	DurationMS int64 `json:"request_duration_ms"`
	SessionID *string `json:"session_id"`
	SessionCount int `json:"session_total_count,omitempty"`
}

func (store *SpendLog) Record(event runtime.InferenceEvent) error {
	if event.Model == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	owner := event.SpendAttribution
	if owner == nil && store.keys != nil {
		store.keys.mu.RLock()
		key := store.keys.state.Keys[event.KeyDigest]
		if key.RotationTarget != "" {
			key = store.keys.state.Keys[key.RotationTarget]
		}
		if key.TeamID == "" && key.ProjectID != "" {
			key.TeamID = store.keys.state.Projects[key.ProjectID].TeamID
		}
		if key.OrgID == "" {
			key.OrgID = store.keys.state.Teams[key.TeamID].OrgID
		}
		store.keys.mu.RUnlock()
		owner = &runtime.SpendAttribution{KeyAlias: key.Alias, UserID: key.UserID, TeamID: key.TeamID, ProjectID: key.ProjectID, OrgID: key.OrgID}
		if event.KeySpendIdentity == "" {
			event.KeySpendIdentity = key.SpendIdentity
		}
	} else if owner == nil {
		owner = &runtime.SpendAttribution{}
	}

	start := time.Unix(0, int64(event.StartTime*1e9)).UTC()
	end := time.Unix(0, int64(event.EndTime*1e9)).UTC()
	agentID := ""
	if event.Provider == "a2a_agent" {
		agentID = event.ModelID
	}
	var firstOutput *float64
	if event.Stream && event.Status == "success" && event.FirstOutput > 0 && event.FirstOutput >= event.StartTime && event.FirstOutput < event.EndTime {
		elapsed := event.FirstOutput - event.StartTime
		firstOutput = &elapsed
	}
	statement := `INSERT INTO minato_proxy_spend
		(request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit,end_user_id,
		cache_read_input_tokens,cache_creation_input_tokens,agent_id,key_spend_identity,api_origin,first_output_seconds,http_status,organization_id,tags,input_messages,response_payload,session_id)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,COALESCE($29::text[],'{}'::text[]),$30::jsonb,$31::jsonb,$32)
		ON CONFLICT (request_id) DO NOTHING`
	args := []any{
		event.ID, start, end, event.KeyDigest, owner.KeyAlias, owner.UserID, owner.TeamID, owner.ProjectID, event.Model, event.ModelGroup,
		event.ModelID, event.Provider, event.CallType, event.Status, event.Prompt, event.Completion, event.Total, event.Spend, event.CacheHit, event.EndUser,
		event.CacheReadInputTokens, event.CacheCreationInputTokens, agentID, event.KeySpendIdentity, event.APISource, firstOutput, event.HTTPStatus, owner.OrgID, pq.Array(event.Tags), nil, nil, spendSessionID(event.SessionID)}
	if store.storePayloads {
		response := event.Response
		if len(response) == 0 {
			response = event.SpendStreamResponse
		}
		args[29], args[30] = spendPayload(event.Messages), spendPayload(response)
	}
	customerCharge := event.EndUser != "" && event.Spend != nil
	keyReset := modelBudgetExpiration(end, event.KeyModelBudgetDuration)
	customerReset := modelBudgetExpiration(end, event.CustomerModelBudgetDuration)
	modelCharge := event.Status == "success" && event.Spend != nil && event.ModelGroup != "" &&
		((event.KeySpendIdentity != "" && !keyReset.IsZero()) || (event.EndUser != "" && !customerReset.IsZero()))
	const updateCustomer = `UPDATE ` + customerTable + ` AS customer
		SET spend = customer.spend + recorded.spend FROM recorded
		WHERE customer.user_id = recorded.end_user_id`
	tagCharge := len(event.Tags) > 0 && event.Spend != nil
	if modelCharge || customerCharge || tagCharge {
		statement = `WITH recorded AS (` + statement + ` RETURNING end_user_id, spend, model_group, tags) `
		if customerCharge {
			statement += `, updated_customer AS (` + updateCustomer + `) `
		}
		if tagCharge {
			statement += `, updated_tags AS (UPDATE ` + tagTable + ` AS tag SET spend = tag.spend + recorded.spend
				FROM recorded WHERE tag.tag_name=ANY(recorded.tags)) `
		}
		if modelCharge {
			statement += `INSERT INTO minato_proxy_key_model_spend AS current_budget
				(key_identity, model_group, duration, reset_at, spend)
				SELECT target.identity, recorded.model_group, target.duration, target.reset_at, recorded.spend
				FROM recorded CROSS JOIN (VALUES ($33::text, $34::text, $35::timestamptz), ($36::text, $37::text, $38::timestamptz))
					AS target(identity, duration, reset_at)
				WHERE target.identity <> '' AND target.reset_at > $39::timestamptz
				ON CONFLICT (key_identity, model_group, duration) DO UPDATE SET
					spend = CASE WHEN current_budget.reset_at <= $39::timestamptz
						THEN EXCLUDED.spend ELSE current_budget.spend + EXCLUDED.spend END,
					reset_at = CASE WHEN current_budget.reset_at <= $39::timestamptz THEN EXCLUDED.reset_at ELSE current_budget.reset_at END`
			customerIdentity := ""
			if event.EndUser != "" {
				customerIdentity = customerModelBudgetIdentity(event.EndUser)
			}
			args = append(args, event.KeySpendIdentity, event.KeyModelBudgetDuration, keyReset,
				customerIdentity, event.CustomerModelBudgetDuration, customerReset, end)
		} else {
			statement += `SELECT 1 FROM recorded`
		}
	}
	_, err := store.db.ExecContext(ctx, statement, args...)
	if err != nil && event.KeyBudgetTracked {
		log.Print("could not record spend for a budgeted key")
	}
	return err
}

// SetStorePayloads configures capture before the spend recorder starts.
func (log *SpendLog) SetStorePayloads(enabled bool) {
	log.storePayloads = enabled
}

func spendPayload(value json.RawMessage) any {
	if len(value) > 64<<10 || !json.Valid(value) {
		return nil
	}
	return string(value)
}

func (log *SpendLog) Start(ctx context.Context) func(runtime.InferenceEvent) {
	queue := make(chan runtime.InferenceEvent, 128)
	go func() {
		for {
			select {
			case event := <-queue:
				log.Record(event)
			case <-ctx.Done():
				return
			}
		}
	}()
	return func(event runtime.InferenceEvent) {
		if !log.storePayloads {
			event.Messages = nil
			event.Response = nil
			event.SpendStreamResponse = nil
		}
		_, providerBudgetTracked := log.providerBudgets[event.Provider]
		if (event.KeyBudgetTracked || providerBudgetTracked) && event.Spend != nil {
			log.Record(event)
			return
		}
		select {
		case queue <- event:
		default:
		}
	}
}

func (log *SpendLog) Handler(next http.Handler, server ...*runtime.Server) http.Handler {
	mux := http.NewServeMux()
	if len(server) > 0 {
		log.agentActivityScope = server[0].GrantedAgentActivityIDs
		mux.HandleFunc("GET /agent/daily/activity", log.agentActivity)
	}
	mux.HandleFunc("GET /tag/daily/activity", log.tagActivity)
	log.registerTagAnalytics(mux)
	mux.HandleFunc("GET /spend/tags", log.spendByTag)
	mux.HandleFunc("GET /global/spend/tags", log.spendByTag)
	mux.HandleFunc("GET /global/spend/report", log.spendReport)
	mux.HandleFunc("GET /global/spend/all_tag_names", log.spendTagNames)
	mux.HandleFunc("GET /customer/daily/activity", log.customerActivity)
	mux.HandleFunc("GET /organization/daily/activity", log.organizationActivity)
	mux.HandleFunc("GET /team/daily/activity", log.teamActivity)
	mux.HandleFunc("GET /user/daily/activity", log.userActivity)
	mux.HandleFunc("GET /user/daily/activity/aggregated", log.userActivity)
	mux.HandleFunc("GET /end_user/daily/activity", log.customerActivity)
	mux.HandleFunc("GET /spend/logs", log.list)
	mux.HandleFunc("GET /spend/logs/v2", log.list)
	mux.HandleFunc("GET /spend/logs/ui", log.list)
	mux.HandleFunc("GET /spend/logs/ui/{request_id}", log.payloadDetail)
	mux.HandleFunc("GET /spend/logs/session/ui", log.list)
	mux.HandleFunc("GET /global/spend", log.globalSpend)
	mux.HandleFunc("POST /global/spend/reset", log.resetGlobalSpend)
	mux.HandleFunc("GET /global/spend/logs", log.daily)
	mux.HandleFunc("GET /global/spend/keys", log.ranked)
	mux.HandleFunc("GET /global/spend/models", log.ranked)
	mux.HandleFunc("GET /global/spend/provider", log.providerSpend)
	mux.HandleFunc("GET /provider/budgets", log.listProviderBudgets)
	mux.HandleFunc("POST /global/spend/end_users", log.endUserSpend)
	mux.HandleFunc("GET /global/spend/teams", log.teamSpend)
	mux.HandleFunc("GET /global/activity", log.globalActivity)
	mux.HandleFunc("GET /global/activity/model", log.globalActivity)
	mux.HandleFunc("GET /global/activity/cache_hits", log.cacheActivity)
	mux.HandleFunc("GET /model/streaming_metrics", log.modelMetrics)
	mux.HandleFunc("GET /model/metrics", log.modelMetrics)
	mux.HandleFunc("GET /model/metrics/slow_responses", log.modelMetrics)
	mux.HandleFunc("GET /model/metrics/exceptions", log.modelMetrics)
	mux.HandleFunc("POST /usage/ai/chat", log.usageAIChat)
	mux.Handle("/", next)
	return mux
}

func (log *SpendLog) effectiveSpendStart(start time.Time) time.Time {
	reset := log.globalSpendResetAt.Load()
	if reset == 0 {
		return start
	}
	cutoff := time.Unix(0, reset).UTC()
	if cutoff.After(start) {
		return cutoff
	}
	return start
}

func (log *SpendLog) resetGlobalSpend(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := log.db.ExecContext(ctx, `DELETE FROM minato_proxy_key_model_spend`); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not reset global spend")
		return
	}
	if err := log.keys.resetImportedSpend(); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not reset global spend")
		return
	}
	log.globalSpendResetAt.Store(time.Now().UTC().UnixNano())
	modelJSON(w, http.StatusOK, map[string]string{
		"message": "Spend for all API Keys and Teams reset successfully",
		"status":  "success",
	})
}

func (log *SpendLog) usageAIChat(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) { return }
	var input struct { Messages []map[string]any `json:"messages"` }
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || len(input.Messages) == 0 { modelError(w, http.StatusBadRequest, "messages are required"); return }
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second); defer cancel()
	var spend float64; var requests int64
	if err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend),0), COUNT(*) FROM minato_proxy_spend WHERE started_at >= ((CURRENT_TIMESTAMP AT TIME ZONE 'UTC')::date - 30)::timestamp AT TIME ZONE 'UTC'`).Scan(&spend, &requests); err != nil { modelError(w, http.StatusInternalServerError, "Could not read usage summary"); return }
	message := fmt.Sprintf("Last 30 days: %.6f spend across %d requests.", spend, requests)
	w.Header().Set("Content-Type", "text/event-stream"); w.Header().Set("Cache-Control", "no-cache"); w.WriteHeader(http.StatusOK)
	chunk := map[string]any{"id": "usage-chat", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": message}, "finish_reason": nil}}}
	body, _ := json.Marshal(chunk); _, _ = w.Write(append(append([]byte("data: "), body...), []byte("\n\n")...)); _, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (log *SpendLog) globalSpend(w http.ResponseWriter, r *http.Request) {
	if !log.keys.requireAdminRead(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var total *float64
	if err := log.db.QueryRowContext(ctx, `SELECT SUM(spend) FROM minato_proxy_spend
		WHERE started_at >= ((CURRENT_TIMESTAMP AT TIME ZONE 'UTC')::date - 30)::timestamp AT TIME ZONE 'UTC'`).
		Scan(&total); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read global spend")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"spend": total, "max_budget": log.maxBudget})
}

func spendTime(raw string, fallback time.Time, inclusiveDay bool) (time.Time, error) {
	if raw == "" {
		return fallback, nil
	}
	if parsed, err := time.Parse(time.DateOnly, raw); err == nil {
		if inclusiveDay {
			parsed = parsed.AddDate(0, 0, 1)
		}
		return parsed, nil
	}
	if parsed, err := time.Parse(time.DateTime, raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func (log *SpendLog) list(w http.ResponseWriter, request *http.Request) {
	scope, ok := log.spendLogScope(w, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	if request.URL.Path == "/spend/logs/session/ui" {
		if query.Get("session_id") == "" {
			modelError(w, http.StatusUnprocessableEntity, "Supply a session ID")
			return
		}
		query.Set("sort_by", "startTime")
		query.Set("sort_order", "asc")
	}
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 50)
	start, startErr := spendTime(query.Get("start_date"), time.Unix(0, 0), false)
	end, endErr := spendTime(query.Get("end_date"), time.Now().UTC(), true)
	if pageErr != nil || sizeErr != nil || startErr != nil || endErr != nil || !start.Before(end) || size > 100 ||
		page-1 > int(^uint(0)>>1)/size {
		modelError(w, http.StatusBadRequest, "Invalid spend log filters or pagination")
		return
	}
	where, args, err := spendFilter(query, start, end)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	where, args = scope.apply(where, args)
	if request.URL.Path == "/spend/logs" && query.Get("start_date") != "" && query.Get("end_date") != "" &&
		query.Get("summarize") != "false" {
		log.legacySpendSummary(w, request, where, args)
		return
	}
	order, ok := spendLogOrder(query)
	if !ok {
		modelError(w, http.StatusBadRequest, "Invalid spend log sort")
		return
	}
	const columns = `request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,
		provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit,end_user_id,
		COALESCE(organization_id,''), COALESCE(agent_id,''), api_origin, first_output_seconds,
		(round(EXTRACT(EPOCH FROM (ended_at - started_at)) * 1000))::bigint, NULLIF(session_id,'')`
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	tx, err := log.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend logs")
		return
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&count); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not count spend logs")
		return
	}
	sizeIndex := strconv.Itoa(len(args) + 1)
	offsetIndex := strconv.Itoa(len(args) + 2)
	rows, err := tx.QueryContext(ctx, "SELECT "+columns+where+" ORDER BY "+order+", request_id LIMIT $"+sizeIndex+
		" OFFSET $"+offsetIndex, append(args, size, (page-1)*size)...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read spend logs")
		return
	}
	defer rows.Close()
	data := make([]spendRow, 0, size)
	for rows.Next() {
		var row spendRow
		if err := rows.Scan(&row.ID, &row.Started, &row.Ended, &row.Key, &row.KeyAlias, &row.User, &row.Team, &row.Project,
			&row.Model, &row.Group, &row.ModelID, &row.Provider, &row.Call, &row.Status, &row.Prompt, &row.Completion, &row.Total, &row.Spend, &row.CacheHit, &row.EndUser,
			&row.Organization, &row.Agent, &row.APIBase, &row.FirstOutputSeconds, &row.DurationMS, &row.SessionID); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode spend logs")
			return
		}
		if row.FirstOutputSeconds != nil {
			firstOutput := row.Started.Add(time.Duration(*row.FirstOutputSeconds * float64(time.Second)))
			row.CompletionStarted = &firstOutput
		}
		data = append(data, row)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading spend logs")
		return
	}
	if request.URL.Path == "/spend/logs" {
		modelJSON(w, http.StatusOK, data)
		return
	}
	if request.URL.Path == "/spend/logs/ui" && len(data) > 0 {
		if err := rows.Close(); err != nil || loadSpendSessionCounts(ctx, tx, scope, data) != nil {
			modelError(w, http.StatusInternalServerError, "Could not count spend log sessions")
			return
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"data": data, "total": count, "total_count": count, "page": page, "page_size": size,
		"total_pages": (count + size - 1) / size})
}

func spendFilter(query url.Values, start, end time.Time) (string, []any, error) {
	args := []any{start, end}
	conditions := []string{"started_at >= $1", "started_at < $2"}
	for _, pair := range [][2]string{{"request_id", "request_id"}, {"session_id", "session_id"}, {"user_id", "user_id"}, {"team_id", "team_id"}, {"project_id", "project_id"},
		{"model", "model"}, {"model_id", "model_id"}, {"status_filter", "status"}, {"api_key", "api_key"}, {"end_user_id", "end_user_id"}, {"end_user", "end_user_id"}} {
		if value := query.Get(pair[0]); value != "" {
			if pair[0] == "api_key" && strings.HasPrefix(value, "sk-") {
				value = keyDigest(value)
			}
			args = append(args, value)
			condition := pair[1] + " = $" + strconv.Itoa(len(args))
			if pair[0] == "status_filter" && value == "success" {
				condition = "(" + condition + " OR status IS NULL OR status = '')"
			}
			conditions = append(conditions, condition)
		}
	}
	if alias := query.Get("key_alias"); alias != "" {
		args = append(args, alias)
		conditions = append(conditions, "strpos(key_alias, $"+strconv.Itoa(len(args))+") > 0")
	}
	if code := query.Get("error_code"); code != "" {
		value, err := strconv.Atoi(code)
		if err != nil || value < 100 || value > 599 {
			return "", nil, errors.New("Invalid error code")
		}
		args = append(args, value)
		conditions = append(conditions, "http_status = $"+strconv.Itoa(len(args)), "status = 'failure'")
	}
	for _, filter := range [][2]string{{"min_spend", ">="}, {"max_spend", "<="}} {
		if raw := query.Get(filter[0]); raw != "" {
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return "", nil, errors.New("Invalid spend filter")
			}
			args = append(args, value)
			conditions = append(conditions, "spend "+filter[1]+" $"+strconv.Itoa(len(args)))
		}
	}
	return " FROM minato_proxy_spend WHERE " + strings.Join(conditions, " AND "), args, nil
}

func (log *SpendLog) daily(w http.ResponseWriter, request *http.Request) {
	scope, ok := log.spendLogScope(w, request)
	if !ok {
		return
	}
	now := time.Now().UTC()
	where, args, err := spendFilter(request.URL.Query(), now.AddDate(0, 0, -30), now)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
	where, args = scope.apply(where, args)
	log.dailyBetween(w, request, where, args)
}

func (log *SpendLog) dailyBetween(w http.ResponseWriter, request *http.Request, where string, args []any) {
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	rows, err := log.db.QueryContext(ctx, `SELECT (started_at AT TIME ZONE 'UTC')::date, sum(spend), sum(total_tokens), count(*)`+
		where+" GROUP BY 1 ORDER BY 1", args...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not aggregate spend logs")
		return
	}
	defer rows.Close()
	type day struct {
		Date string `json:"date"`
		Spend *float64 `json:"spend"`
		Tokens int `json:"total_tokens"`
		Requests int `json:"total_requests"`
	}
	data := []day{}
	for rows.Next() {
		var entry day
		var date time.Time
		if err := rows.Scan(&date, &entry.Spend, &entry.Tokens, &entry.Requests); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode spend totals")
			return
		}
		entry.Date = date.Format(time.DateOnly)
		data = append(data, entry)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading spend totals")
		return
	}
	modelJSON(w, http.StatusOK, data)
}

func (log *SpendLog) teamMemberSpendSince(ctx context.Context, team, user string, start time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start = log.effectiveSpendStart(start)
	var spend float64
	err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend), 0) FROM minato_proxy_spend WHERE team_id=$1 AND user_id=$2 AND started_at >= $3`, team, user, start).Scan(&spend)
	return spend, err
}

func (registry *KeyRegistry) SetTeamMemberSpend(log *SpendLog) {
	if log != nil {
		registry.teamMemberSpend = log.teamMemberSpendSince
		registry.userSpend = log.UserSpend
	}
}

func (log *SpendLog) UserSpend(ctx context.Context, user string, start time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start = log.effectiveSpendStart(start)
	var spend float64
	err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend), 0) FROM minato_proxy_spend WHERE user_id=$1 AND started_at >= $2`, user, start).Scan(&spend)
	return spend, err
}

func (log *SpendLog) TeamSpend(ctx context.Context, teamID string, start time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start = log.effectiveSpendStart(start)
	var spend float64
	err := log.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(spend), 0) FROM minato_proxy_spend WHERE team_id=$1 AND started_at >= $2`, teamID, start).Scan(&spend)
	return spend, err
}

func (log *SpendLog) budgetMaximum(ctx context.Context, id string) (*float64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var max *float64
	err := log.db.QueryRowContext(ctx, `SELECT max_budget FROM `+budgetTable+` WHERE budget_id=$1`, id).Scan(&max)
	return max, err
}

type budgetPeriod struct {
	Max *float64
	RPM *int
	TPM *int
	Parallel *int
	Start time.Time
}

func (log *SpendLog) budgetPeriod(ctx context.Context, id string) (budgetPeriod, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var period budgetPeriod
	var start sql.NullTime
	err := log.db.QueryRowContext(ctx, `SELECT max_budget, budget_period_started_at, rpm_limit, tpm_limit, max_parallel_requests
		FROM `+budgetTable+` WHERE budget_id=$1`, id).Scan(&period.Max, &start, &period.RPM, &period.TPM, &period.Parallel)
	period.Start = start.Time
	return period, err
}
