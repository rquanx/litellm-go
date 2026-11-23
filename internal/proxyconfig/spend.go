package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type SpendLog struct {
	db   *sql.DB
	keys *KeyRegistry
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
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_end_user ON minato_proxy_spend (end_user_id, started_at) WHERE end_user_id <> '';
	CREATE INDEX IF NOT EXISTS minato_proxy_spend_started_at ON minato_proxy_spend (started_at DESC, request_id);`)
	if err != nil {
		return nil, errors.New("could not initialize Go proxy spend logs")
	}
	return &SpendLog{db: db, keys: keys}, nil
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
}

func (log *SpendLog) Record(event runtime.InferenceEvent) {
	if event.Model == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	log.keys.mu.RLock()
	key := log.keys.state.Keys[event.KeyDigest]
	if key.RotationTarget != "" {
		key = log.keys.state.Keys[key.RotationTarget]
	}
	if key.TeamID == "" && key.ProjectID != "" {
		key.TeamID = log.keys.state.Projects[key.ProjectID].TeamID
	}
	log.keys.mu.RUnlock()

	start := time.Unix(0, int64(event.StartTime*1e9)).UTC()
	end := time.Unix(0, int64(event.EndTime*1e9)).UTC()
	_, _ = log.db.ExecContext(ctx, `INSERT INTO minato_proxy_spend
		(request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit,end_user_id,
		cache_read_input_tokens,cache_creation_input_tokens)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (request_id) DO NOTHING`,
		event.ID, start, end, event.KeyDigest, key.Alias, key.UserID, key.TeamID, key.ProjectID, event.Model, event.ModelGroup,
		event.ModelID, event.Provider, event.CallType, event.Status, event.Prompt, event.Completion, event.Total, event.Spend, event.CacheHit, event.EndUser,
		event.CacheReadInputTokens, event.CacheCreationInputTokens)
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
		event.Messages = nil
		event.Response = nil
		select {
		case queue <- event:
		default:
		}
	}
}

func (log *SpendLog) Handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /customer/daily/activity", log.customerActivity)
	mux.HandleFunc("GET /end_user/daily/activity", log.customerActivity)
	mux.HandleFunc("GET /spend/logs", log.list)
	mux.HandleFunc("GET /spend/logs/v2", log.list)
	mux.HandleFunc("GET /global/spend/logs", log.daily)
	mux.HandleFunc("GET /global/spend/keys", log.ranked)
	mux.HandleFunc("GET /global/spend/models", log.ranked)
	mux.HandleFunc("GET /global/spend/teams", log.teamSpend)
	mux.HandleFunc("GET /global/activity/cache_hits", log.cacheActivity)
	mux.Handle("/", next)
	return mux
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
	return time.Parse(time.RFC3339, raw)
}

func (log *SpendLog) list(w http.ResponseWriter, request *http.Request) {
	if !log.keys.requireMaster(w, request) {
		return
	}
	query := request.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 50)
	start, startErr := spendTime(query.Get("start_date"), time.Unix(0, 0), false)
	end, endErr := spendTime(query.Get("end_date"), time.Now().UTC(), true)
	if pageErr != nil || sizeErr != nil || startErr != nil || endErr != nil || !start.Before(end) || size > 100 ||
		page-1 > int(^uint(0)>>1)/size {
		modelError(w, http.StatusBadRequest, "Invalid spend log filters or pagination")
		return
	}
	if request.URL.Path == "/spend/logs" && query.Get("start_date") != "" && query.Get("end_date") != "" &&
		query.Get("summarize") != "false" {
		log.dailyBetween(w, request, start, end)
		return
	}
	const columns = `request_id,started_at,ended_at,api_key,key_alias,user_id,team_id,project_id,model,model_group,model_id,
		provider,call_type,status,prompt_tokens,completion_tokens,total_tokens,spend,cache_hit,end_user_id`
	where, args, err := spendFilter(query, start, end)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	rows, err := tx.QueryContext(ctx, "SELECT "+columns+where+" ORDER BY started_at DESC, request_id LIMIT $"+sizeIndex+
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
			&row.Model, &row.Group, &row.ModelID, &row.Provider, &row.Call, &row.Status, &row.Prompt, &row.Completion, &row.Total, &row.Spend, &row.CacheHit, &row.EndUser); err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode spend logs")
			return
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
	modelJSON(w, http.StatusOK, map[string]any{"data": data, "total_count": count, "page": page, "page_size": size,
		"total_pages": (count + size - 1) / size})
}

func spendFilter(query url.Values, start, end time.Time) (string, []any, error) {
	args := []any{start, end}
	conditions := []string{"started_at >= $1", "started_at < $2"}
	for _, pair := range [][2]string{{"request_id", "request_id"}, {"user_id", "user_id"}, {"team_id", "team_id"}, {"project_id", "project_id"},
		{"model", "model"}, {"model_id", "model_id"}, {"key_alias", "key_alias"}, {"status_filter", "status"}, {"api_key", "api_key"}, {"end_user_id", "end_user_id"}} {
		if value := query.Get(pair[0]); value != "" {
			if pair[0] == "api_key" && strings.HasPrefix(value, "sk-") {
				value = keyDigest(value)
			}
			args = append(args, value)
			conditions = append(conditions, pair[1]+" = $"+strconv.Itoa(len(args)))
		}
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
	if !log.keys.requireMaster(w, request) {
		return
	}
	now := time.Now().UTC()
	log.dailyBetween(w, request, now.AddDate(0, 0, -30), now)
}

func (log *SpendLog) dailyBetween(w http.ResponseWriter, request *http.Request, start, end time.Time) {
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	where, args, err := spendFilter(request.URL.Query(), start, end)
	if err != nil {
		modelError(w, http.StatusBadRequest, err.Error())
		return
	}
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
