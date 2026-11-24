package proxyconfig

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

const (
	workflowRunTable = `"LiteLLM_WorkflowRun"`
	workflowEventTable = `"LiteLLM_WorkflowEvent"`
	workflowMessageTable = `"LiteLLM_WorkflowMessage"`
	workflowScope = `($1 OR created_by = NULLIF($2,''))`
)

type workflowAPI struct {
	db *sql.DB
	keys *KeyRegistry
}

func WithWorkflowManagement(next http.Handler, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	api := workflowAPI{db: db, keys: keys}
	if db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS "LiteLLM_WorkflowRun" (
				run_id TEXT PRIMARY KEY, session_id TEXT NOT NULL UNIQUE, workflow_type TEXT NOT NULL,
				status TEXT NOT NULL DEFAULT 'pending', created_by TEXT,
				created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMP(3) NOT NULL, input JSONB, output JSONB, metadata JSONB
			);
			CREATE INDEX IF NOT EXISTS "LiteLLM_WorkflowRun_workflow_type_status_idx" ON "LiteLLM_WorkflowRun"(workflow_type,status);
			CREATE INDEX IF NOT EXISTS "LiteLLM_WorkflowRun_created_at_idx" ON "LiteLLM_WorkflowRun"(created_at);
			CREATE INDEX IF NOT EXISTS "LiteLLM_WorkflowRun_created_by_idx" ON "LiteLLM_WorkflowRun"(created_by);
			CREATE TABLE IF NOT EXISTS "LiteLLM_WorkflowEvent" (
				event_id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES "LiteLLM_WorkflowRun"(run_id) ON UPDATE CASCADE,
				event_type TEXT NOT NULL, step_name TEXT NOT NULL, sequence_number INTEGER NOT NULL,
				data JSONB, created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(run_id, sequence_number)
			);
			CREATE TABLE IF NOT EXISTS "LiteLLM_WorkflowMessage" (
				message_id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES "LiteLLM_WorkflowRun"(run_id) ON UPDATE CASCADE,
				role TEXT NOT NULL, content TEXT NOT NULL, sequence_number INTEGER NOT NULL, session_id TEXT,
				created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(run_id, sequence_number)
			);`); err != nil {
			return nil, errors.New("could not initialize workflow storage")
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workflows/runs", api.serve)
	mux.HandleFunc("POST /v1/workflows/runs", api.serve)
	mux.HandleFunc("GET /v1/workflows/runs/{run_id}", api.serve)
	mux.HandleFunc("PATCH /v1/workflows/runs/{run_id}", api.serve)
	mux.HandleFunc("GET /v1/workflows/runs/{run_id}/events", api.serve)
	mux.HandleFunc("POST /v1/workflows/runs/{run_id}/events", api.serve)
	mux.HandleFunc("GET /v1/workflows/runs/{run_id}/messages", api.serve)
	mux.HandleFunc("POST /v1/workflows/runs/{run_id}/messages", api.serve)
	mux.Handle("/", next)
	return mux, nil
}

func (api workflowAPI) serve(w http.ResponseWriter, r *http.Request) {
	token := runtime.ProxyCredential(r)
	admin := api.keys.master != "" && subtle.ConstantTimeCompare([]byte(token), []byte(api.keys.master)) == 1
	if !admin {
		if _, valid := api.keys.Authorize(token); !valid {
			workflowError(w, http.StatusUnauthorized, "Valid proxy credentials are required")
			return
		}
	}
	if api.db == nil {
		workflowError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	owner := ""
	if !admin {
		owner = keyDigest(token)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	id := r.PathValue("run_id")
	var err error
	switch r.Pattern {
	case "GET /v1/workflows/runs":
		err = api.list(w, r, admin, owner)
	case "POST /v1/workflows/runs":
		err = api.create(w, r, owner)
	case "PATCH /v1/workflows/runs/{run_id}":
		err = api.update(w, r, admin, owner, id)
	case "GET /v1/workflows/runs/{run_id}/events", "POST /v1/workflows/runs/{run_id}/events":
		err = api.children(w, r, admin, owner, id, true)
	case "GET /v1/workflows/runs/{run_id}/messages", "POST /v1/workflows/runs/{run_id}/messages":
		err = api.children(w, r, admin, owner, id, false)
	default:
		err = workflowResponse(w, api.db.QueryRowContext(ctx, `SELECT to_jsonb(r) || jsonb_build_object('events',
			COALESCE((SELECT jsonb_agg(e) FROM (SELECT * FROM `+workflowEventTable+`
			WHERE run_id=$3 ORDER BY sequence_number DESC LIMIT 1) e),'[]'::jsonb)) FROM `+workflowRunTable+` r
			WHERE `+workflowScope+` AND run_id=$3`, admin, owner, id))
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		workflowError(w, http.StatusNotFound, "Run '"+id+"' not found")
	case err != nil:
		workflowError(w, http.StatusInternalServerError, "Could not access workflow storage")
	}
}

func workflowError(w http.ResponseWriter, status int, detail string) {
	modelJSON(w, status, map[string]string{"detail": detail})
}

func workflowResponse(w http.ResponseWriter, row *sql.Row) error {
	var body json.RawMessage
	if err := row.Scan(&body); err != nil {
		return err
	}
	modelJSON(w, http.StatusOK, body)
	return nil
}

// Workflow payloads are decoded as JSON objects before they reach PostgreSQL.
func workflowObject(value map[string]json.RawMessage) any {
	if value == nil {
		return nil
	}
	body, _ := json.Marshal(value)
	return string(body)
}

func (api workflowAPI) create(w http.ResponseWriter, r *http.Request, owner string) error {
	var input struct {
		WorkflowType *string `json:"workflow_type"`
		Input map[string]json.RawMessage `json:"input"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.WorkflowType == nil {
		workflowError(w, http.StatusUnprocessableEntity, "workflow_type is required")
		return nil
	}
	return workflowResponse(w, api.db.QueryRowContext(r.Context(), `INSERT INTO `+workflowRunTable+` AS r
		(run_id,session_id,workflow_type,created_by,input,metadata,updated_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5::jsonb,$6::jsonb,CURRENT_TIMESTAMP) RETURNING to_jsonb(r)`,
		newEntityID(), newEntityID(), *input.WorkflowType, owner, workflowObject(input.Input), workflowObject(input.Metadata)))
}

func (api workflowAPI) list(w http.ResponseWriter, r *http.Request, admin bool, owner string) error {
	query := r.URL.Query()
	limit, err := keyListInt(query, "limit", 50)
	if err != nil || limit > 250 {
		workflowError(w, http.StatusUnprocessableEntity, "limit must be between 1 and 250")
		return nil
	}
	var statuses []string
	if value := query.Get("status"); value != "" {
		statuses = strings.Split(value, ",")
		for i := range statuses {
			statuses[i] = strings.TrimSpace(statuses[i])
		}
	}
	return workflowResponse(w, api.db.QueryRowContext(r.Context(), `SELECT jsonb_build_object(
		'runs',COALESCE(jsonb_agg(r ORDER BY created_at DESC, run_id),'[]'::jsonb),'count',count(*))
		FROM (SELECT * FROM `+workflowRunTable+` WHERE `+workflowScope+`
			AND ($3='' OR workflow_type=$3) AND ($4::text[] IS NULL OR status=ANY($4))
			ORDER BY created_at DESC,run_id LIMIT $5) r`, admin, owner, query.Get("workflow_type"), pq.Array(statuses), limit))
}

func (api workflowAPI) update(w http.ResponseWriter, r *http.Request, admin bool, owner, id string) error {
	var input struct {
		Status *string `json:"status"`
		Output map[string]json.RawMessage `json:"output"`
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		workflowError(w, http.StatusUnprocessableEntity, "Invalid workflow update")
		return nil
	}
	if input.Status != nil {
		switch *input.Status {
		case "pending", "running", "paused", "completed", "failed":
		default:
			workflowError(w, http.StatusUnprocessableEntity, "Invalid workflow status")
			return nil
		}
	}
	if input.Status == nil && input.Output == nil && input.Metadata == nil {
		workflowError(w, http.StatusBadRequest, "No fields to update")
		return nil
	}
	return workflowResponse(w, api.db.QueryRowContext(r.Context(), `UPDATE `+workflowRunTable+` AS r SET
		status=COALESCE(NULLIF($4,''),status),output=COALESCE($5::jsonb,output),metadata=COALESCE($6::jsonb,metadata),
		updated_at=CURRENT_TIMESTAMP WHERE `+workflowScope+` AND run_id=$3 RETURNING to_jsonb(r)`,
		admin, owner, id, input.Status, workflowObject(input.Output), workflowObject(input.Metadata)))
}

func (api workflowAPI) children(w http.ResponseWriter, r *http.Request, admin bool, owner, id string, events bool) error {
	table, collection := workflowMessageTable, "messages"
	if events {
		table, collection = workflowEventTable, "events"
	}
	if r.Method != http.MethodPost {
		limit, err := keyListInt(r.URL.Query(), "limit", 100)
		if err != nil || limit > 500 {
			workflowError(w, http.StatusUnprocessableEntity, "limit must be between 1 and 500")
			return nil
		}
		return workflowResponse(w, api.db.QueryRowContext(r.Context(), `SELECT (
			SELECT jsonb_build_object($5::text,COALESCE(jsonb_agg(c ORDER BY sequence_number),'[]'::jsonb),'count',count(*))
			FROM (SELECT * FROM `+table+` WHERE run_id=$3 ORDER BY sequence_number LIMIT $4) c
			) FROM `+workflowRunTable+` WHERE `+workflowScope+` AND run_id=$3`, admin, owner, id, limit, collection))
	}
	var data map[string]json.RawMessage
	var first, second *string
	var session *string
	var status string
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	if events {
		var input struct {
			Type *string `json:"event_type"`
			Step *string `json:"step_name"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := decodeModelJSON(body, &input); err == nil {
			first, second, data = input.Type, input.Step, input.Data
			if first != nil {
				switch *first {
				case "step.started", "hook.received":
					status = "running"
				case "step.failed":
					status = "failed"
				case "hook.waiting":
					status = "paused"
				}
			}
		}
	} else {
		var input struct {
			Role *string `json:"role"`
			Content *string `json:"content"`
			Session *string `json:"session_id"`
		}
		if err := decodeModelJSON(body, &input); err == nil {
			first, second, session = input.Role, input.Content, input.Session
		}
	}
	if first == nil || second == nil {
		workflowError(w, http.StatusUnprocessableEntity, "Missing workflow "+collection+" fields")
		return nil
	}
	ctx := r.Context()
	tx, err := api.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize appends per run. The unique sequence constraint also covers writers using the upstream API.
	var runID string
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM `+workflowRunTable+` WHERE `+workflowScope+` AND run_id=$3 FOR UPDATE`, admin, owner, id).Scan(&runID); err != nil {
		return err
	}
	fields := "message_id,run_id,role,content,session_id,sequence_number"
	var extra any = session
	if events {
		fields = "event_id,run_id,event_type,step_name,data,sequence_number"
		extra = workflowObject(data)
	}
	var response json.RawMessage
	if err := tx.QueryRowContext(ctx, `INSERT INTO `+table+` AS c (`+fields+`)
		VALUES ($1,$2,$3,$4,$5,(SELECT COALESCE(max(sequence_number)+1,0) FROM `+table+` WHERE run_id=$2))
		ON CONFLICT (run_id,sequence_number) DO NOTHING RETURNING to_jsonb(c)`,
		newEntityID(), id, *first, *second, extra).Scan(&response); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			workflowError(w, http.StatusConflict, "Concurrent write conflict. Please retry.")
			return nil
		}
		return err
	}
	if status != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE `+workflowRunTable+` SET status=$2,updated_at=CURRENT_TIMESTAMP WHERE run_id=$1`, id, status); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	modelJSON(w, http.StatusOK, response)
	return nil
}
