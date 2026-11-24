package proxyconfig

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

const guardrailMetricsTable = `"LiteLLM_DailyGuardrailMetrics"`
const policyMetricsTable = `"LiteLLM_DailyPolicyMetrics"`
const guardrailRunTable = `minato_guardrail_runs`

type GuardrailUsage struct {
	db   *sql.DB
	keys *KeyRegistry
}

func OpenGuardrailUsage(db *sql.DB, keys *KeyRegistry) (*GuardrailUsage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, table := range []struct{ name, id string }{{guardrailMetricsTable, "guardrail_id"}, {policyMetricsTable, "policy_id"}} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table.name+` (
		`+table.id+` TEXT NOT NULL, date TEXT NOT NULL,
		requests_evaluated BIGINT NOT NULL DEFAULT 0, passed_count BIGINT NOT NULL DEFAULT 0,
		blocked_count BIGINT NOT NULL DEFAULT 0, flagged_count BIGINT NOT NULL DEFAULT 0,
		avg_score DOUBLE PRECISION, avg_latency_ms DOUBLE PRECISION,
		created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		PRIMARY KEY (`+table.id+`,date))`); err != nil {
			return nil, fmt.Errorf("could not initialize guardrail metrics: %w", err)
		}
	}
	_, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS "LiteLLM_DailyGuardrailMetrics_date_idx" ON `+guardrailMetricsTable+`(date);
		CREATE INDEX IF NOT EXISTS "LiteLLM_DailyPolicyMetrics_date_idx" ON `+policyMetricsTable+`(date);
		CREATE TABLE IF NOT EXISTS `+guardrailRunTable+` (
			id TEXT PRIMARY KEY, guardrail_name TEXT NOT NULL, started_at TIMESTAMPTZ NOT NULL,
			action TEXT NOT NULL, model TEXT NOT NULL, latency_ms DOUBLE PRECISION NOT NULL
		);
		ALTER TABLE `+guardrailRunTable+` ADD COLUMN IF NOT EXISTS policy_id TEXT NOT NULL DEFAULT '';
		CREATE INDEX IF NOT EXISTS minato_guardrail_runs_name_time ON `+guardrailRunTable+`(guardrail_name,started_at DESC,id);
		CREATE INDEX IF NOT EXISTS minato_guardrail_runs_policy_time ON `+guardrailRunTable+`(policy_id,started_at DESC,id) WHERE policy_id<>''`)
	if err != nil {
		return nil, fmt.Errorf("could not initialize guardrail metrics: %w", err)
	}
	return &GuardrailUsage{db: db, keys: keys}, nil
}

func (usage *GuardrailUsage) Start(ctx context.Context) func(runtime.GuardrailEvent) {
	queue := make(chan runtime.GuardrailEvent, 256)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-queue:
				recordCtx, cancel := context.WithTimeout(ctx, time.Second)
				_ = usage.record(recordCtx, event)
				cancel()
			}
		}
	}()
	return func(event runtime.GuardrailEvent) {
		select {
		case queue <- event:
		default:
		}
	}
}

func (usage *GuardrailUsage) record(ctx context.Context, event runtime.GuardrailEvent) error {
	table, key, id := guardrailMetricsTable, "guardrail_id", event.Name
	if event.Name == "" {
		if event.PolicyID == "" {
			return nil
		}
		table, key, id = policyMetricsTable, "policy_id", event.PolicyID
	}
	_, err := usage.db.ExecContext(ctx, `WITH run AS (
		INSERT INTO `+guardrailRunTable+`(id,guardrail_name,started_at,action,model,latency_ms,policy_id)
		VALUES($5,$8,$6,$3,$7,$4,$9)
	)
	INSERT INTO `+table+` AS m
		(`+key+`,date,requests_evaluated,passed_count,blocked_count,flagged_count,avg_latency_ms)
		VALUES($1,$2,1,($3='passed')::int,($3='blocked')::int,($3='flagged')::int,$4)
		ON CONFLICT (`+key+`,date) DO UPDATE SET requests_evaluated=m.requests_evaluated+1,
		passed_count=m.passed_count+EXCLUDED.passed_count,blocked_count=m.blocked_count+EXCLUDED.blocked_count,
		flagged_count=m.flagged_count+EXCLUDED.flagged_count,
		avg_latency_ms=(COALESCE(m.avg_latency_ms,EXCLUDED.avg_latency_ms)*m.requests_evaluated+EXCLUDED.avg_latency_ms)/(m.requests_evaluated+1),
		updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'`, id, event.At.UTC().Format(time.DateOnly), event.Outcome,
		float64(event.Duration)/float64(time.Millisecond), newEntityID(), event.At, event.Model, event.Name, event.PolicyID)
	return err
}

type guardrailMetricCounts struct {
	evaluated int64
	passed    int64
	blocked   int64
}

func (counts *guardrailMetricCounts) add(in guardrailMetricCounts) {
	counts.evaluated += in.evaluated
	counts.passed += in.passed
	counts.blocked += in.blocked
}

func (counts guardrailMetricCounts) failRate() float64 {
	if counts.evaluated == 0 {
		return 0
	}
	return 100 * float64(counts.blocked) / float64(counts.evaluated)
}

type guardrailMetricItem struct {
	id, name, provider, kind string
	description              any
}

func (usage *GuardrailUsage) Handler(next http.Handler, static []runtime.GenericGuardrail) http.Handler {
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request) {
		if usage.keys.requireMaster(w, r) {
			usage.serve(w, r, static)
		}
	}
	mux.HandleFunc("GET /guardrails/usage/overview", serve)
	mux.HandleFunc("GET /policies/usage/overview", serve)
	mux.HandleFunc("GET /guardrails/usage/detail/{guardrail_id}", serve)
	mux.HandleFunc("GET /guardrails/usage/logs", func(w http.ResponseWriter, r *http.Request) {
		if usage.keys.requireMaster(w, r) {
			usage.readRunLogs(w, r)
		}
	})
	mux.Handle("/", next)
	return mux
}

func guardrailUsageDates(r *http.Request, now time.Time) (time.Time, time.Time, error) {
	start, end := now.UTC().AddDate(0, 0, -7), now.UTC()
	query := r.URL.Query()
	if value := query.Get("start_date"); value != "" {
		date, err := time.Parse(time.DateOnly, value)
		if err != nil {
			return start, end, err
		}
		start = date
	}
	if value := query.Get("end_date"); value != "" {
		date, err := time.Parse(time.DateOnly, value)
		if err != nil {
			return start, end, err
		}
		end = date
	}
	if start.Format(time.DateOnly) > end.Format(time.DateOnly) {
		return start, end, fmt.Errorf("start date follows end date")
	}
	return start, end, nil
}

func (usage *GuardrailUsage) serve(w http.ResponseWriter, r *http.Request, static []runtime.GenericGuardrail) {
	start, end, err := guardrailUsageDates(r, time.Now())
	if err != nil {
		modelError(w, 400, "Expected an ordered YYYY-MM-DD date range")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	policy := r.URL.Path == "/policies/usage/overview"
	var items map[string]guardrailMetricItem
	if policy {
		items, err = usage.policyItems(ctx)
	} else {
		items, err = usage.usageItems(ctx, static)
	}
	if err != nil {
		modelError(w, 500, "Could not read guardrails")
		return
	}
	detailID := r.PathValue("guardrail_id")
	previousStart := start.AddDate(0, 0, -7).Format(time.DateOnly)
	if detailID != "" {
		item, ok := items[detailID]
		if !ok {
			modelError(w, 404, "Guardrail not found")
			return
		}
		items = map[string]guardrailMetricItem{detailID: item}
		previousStart = ""
	}
	usage.report(ctx, w, items, detailID, start.Format(time.DateOnly), end.Format(time.DateOnly), previousStart, policy)
}

func (usage *GuardrailUsage) policyItems(ctx context.Context) (map[string]guardrailMetricItem, error) {
	rows, err := usage.db.QueryContext(ctx, `SELECT policy_id,policy_name FROM `+policyTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]guardrailMetricItem)
	for rows.Next() {
		item := guardrailMetricItem{kind: "Policy", provider: "LiteLLM"}
		if err := rows.Scan(&item.id, &item.name); err != nil {
			return nil, err
		}
		items[item.id] = item
	}
	return items, rows.Err()
}

func (usage *GuardrailUsage) usageItems(ctx context.Context, static []runtime.GenericGuardrail) (map[string]guardrailMetricItem, error) {
	items := make(map[string]guardrailMetricItem)
	for _, guard := range static {
		items[guard.Name] = guardrailMetricItem{id: guard.Name, name: guard.Name, provider: guard.Provider, kind: "Guardrail"}
	}
	rows, err := usage.db.QueryContext(ctx, `SELECT guardrail_id,guardrail_name,litellm_params,guardrail_info FROM `+guardrailTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item guardrailMetricItem
		var params, info []byte
		if err := rows.Scan(&item.id, &item.name, &params, &info); err != nil {
			return nil, err
		}
		var metadata map[string]any
		_ = decodePolicyCondition(params, &metadata)
		item.provider, _ = metadata["guardrail"].(string)
		metadata = nil
		_ = decodePolicyCondition(info, &metadata)
		item.kind, _ = metadata["type"].(string)
		item.description = metadata["description"]
		items[item.id] = item
	}
	return items, rows.Err()
}

func (usage *GuardrailUsage) report(ctx context.Context, w http.ResponseWriter, items map[string]guardrailMetricItem, detailID, start, end, previousStart string, policy bool) {
	table, key := guardrailMetricsTable, "guardrail_id"
	if policy {
		table, key = policyMetricsTable, "policy_id"
	}
	rows, err := usage.db.QueryContext(ctx, `SELECT `+key+`,date,COALESCE(requests_evaluated,0),COALESCE(passed_count,0),COALESCE(blocked_count,0)
		FROM `+table+` WHERE date >= $1 AND date <= $2 AND ($3='' OR `+key+` IN ($3,$4))`,
		previousStart, end, detailID, items[detailID].name)
	if err != nil {
		modelError(w, 500, "Could not read guardrail metrics")
		return
	}
	defer rows.Close()
	current, previous, chart := map[string]guardrailMetricCounts{}, map[string]guardrailMetricCounts{}, map[string]guardrailMetricCounts{}
	var total guardrailMetricCounts
	for rows.Next() {
		var id, date string
		var count guardrailMetricCounts
		if err := rows.Scan(&id, &date, &count.evaluated, &count.passed, &count.blocked); err != nil {
			modelError(w, 500, "Could not read guardrail metrics")
			return
		}
		if date < start {
			addGuardrailMetrics(previous, id, count)
			continue
		}
		addGuardrailMetrics(chart, date, count)
		addGuardrailMetrics(current, id, count)
		total.add(count)
	}
	if rows.Err() != nil {
		modelError(w, 500, "Could not read guardrail metrics")
		return
	}
	timeSeries := []map[string]any{}
	for _, date := range slices.Sorted(maps.Keys(chart)) {
		point := map[string]any{"date": date, "passed": chart[date].passed, "blocked": chart[date].blocked}
		if detailID != "" {
			point["score"] = nil
		}
		timeSeries = append(timeSeries, point)
	}
	if detailID != "" {
		item := items[detailID]
		older := previous[detailID]
		if item.name != detailID {
			older.add(previous[item.name])
		}
		result := guardrailUsageRow(item, total, older.failRate())
		delete(result, "id")
		delete(result, "name")
		result["guardrail_id"], result["guardrail_name"], result["description"], result["time_series"] = item.id, item.name, item.description, timeSeries
		modelJSON(w, 200, result)
		return
	}
	result := []map[string]any{}
	covered := make(map[string]bool)
	for _, id := range slices.Sorted(maps.Keys(items)) {
		item := items[id]
		covered[id], covered[item.name] = true, true
		count, found := current[item.name]
		if !found || policy {
			count = current[id]
		}
		prior, found := previous[item.name]
		if !found || policy {
			prior = previous[id]
		}
		result = append(result, guardrailUsageRow(item, count, prior.failRate()))
	}
	for _, id := range slices.Sorted(maps.Keys(current)) {
		if !covered[id] && current[id].evaluated > 0 {
			kind, provider := "Guardrail", "Custom"
			if policy {
				kind, provider = "Policy", "LiteLLM"
			}
			result = append(result, guardrailUsageRow(guardrailMetricItem{id: id, name: id, provider: provider, kind: kind}, current[id], previous[id].failRate()))
		}
	}
	modelJSON(w, 200, map[string]any{"rows": result, "chart": timeSeries, "totalRequests": total.evaluated, "totalBlocked": total.blocked, "passRate": math.Round((100-total.failRate())*10) / 10})
}

func addGuardrailMetrics(target map[string]guardrailMetricCounts, key string, count guardrailMetricCounts) {
	sum := target[key]
	sum.add(count)
	target[key] = sum
}

func guardrailUsageRow(item guardrailMetricItem, count guardrailMetricCounts, previous float64) map[string]any {
	if item.kind == "" {
		item.kind = "Guardrail"
	}
	if item.provider == "" {
		item.provider = "Unknown"
	}
	rate := count.failRate()
	status, trend := "healthy", "stable"
	if rate > 15 {
		status = "critical"
	} else if rate > 5 {
		status = "warning"
	}
	if previous > 0 {
		if rate-previous > 0.5 {
			trend = "up"
		} else if previous-rate > 0.5 {
			trend = "down"
		}
	}
	return map[string]any{"id": item.id, "name": item.name, "type": item.kind, "provider": item.provider, "requestsEvaluated": count.evaluated,
		"failRate": math.Round(rate*10) / 10, "avgScore": nil, "avgLatency": nil, "status": status, "trend": trend}
}

func (usage *GuardrailUsage) readRunLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, size := 1, 50
	for name, target := range map[string]*int{"page": &page, "page_size": &size} {
		if value := query.Get(name); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 1 || parsed > math.MaxInt32 || (name == "page_size" && parsed > 100) {
				modelError(w, 422, "Invalid pagination")
				return
			}
			*target = parsed
		}
	}
	result := []map[string]any{}
	total := int64(0)
	id := query.Get("guardrail_id")
	policyID := query.Get("policy_id")
	if id != "" || policyID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var start, end *time.Time
		for name, target := range map[string]**time.Time{"start_date": &start, "end_date": &end} {
			value := query.Get(name)
			if value == "" {
				continue
			}
			date, err := time.Parse(time.DateOnly, value)
			if err == nil && name == "end_date" {
				date = date.AddDate(0, 0, 1).Add(-time.Microsecond)
			}
			if err != nil {
				date, err = time.Parse(time.RFC3339, value)
			}
			if err != nil {
				modelError(w, 422, "Expected a date or timestamp")
				return
			}
			*target = &date
		}
		args := []any{id, query.Get("action"), start, end, policyID}
		tx, err := usage.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			modelError(w, 500, "Could not read guardrail runs")
			return
		}
		defer tx.Rollback()
		source, err := guardrailRunSource(ctx, tx)
		if err != nil {
			modelError(w, 500, "Could not locate guardrail run storage")
			return
		}
		const filter = ` FROM runs WHERE ($2='' OR action=$2)`
		if err := tx.QueryRowContext(ctx, source+`SELECT COUNT(*)`+filter, args...).Scan(&total); err != nil {
			modelError(w, 500, "Could not count guardrail runs")
			return
		}
		rows, err := tx.QueryContext(ctx, source+`SELECT id,started_at,action,model,latency_ms,score,input_snippet,output_snippet,reason`+filter+
			` ORDER BY started_at DESC,id LIMIT $6 OFFSET $7`, append(args, size, int64(page-1)*int64(size))...)
		if err != nil {
			modelError(w, 500, "Could not read guardrail runs")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var runID, action, model string
			var at time.Time
			var latency, score *float64
			var input, output, reason *string
			if err := rows.Scan(&runID, &at, &action, &model, &latency, &score, &input, &output, &reason); err != nil {
				modelError(w, 500, "Could not read guardrail run")
				return
			}
			var latencyMS any
			if latency != nil {
				latencyMS = math.Round(*latency)
			}
			result = append(result, map[string]any{"id": runID, "timestamp": at.UTC(), "action": action, "model": model,
				"latency_ms": latencyMS, "score": score, "input_snippet": input, "output_snippet": output, "reason": reason})
		}
		if rows.Err() != nil || tx.Commit() != nil {
			modelError(w, 500, "Could not read guardrail runs")
			return
		}
	}
	modelJSON(w, 200, map[string]any{"logs": result, "total": total, "page": page, "page_size": size})
}
