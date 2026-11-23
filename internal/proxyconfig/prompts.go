package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

const promptTable = `"LiteLLM_PromptTable"`

type promptAPI struct {
	db   *sql.DB
	keys *KeyRegistry
}

type promptRecord struct {
	PromptID    string         `json:"prompt_id"`
	Version     int            `json:"version"`
	Environment string         `json:"environment"`
	CreatedBy   *string        `json:"created_by"`
	Created     time.Time      `json:"created_at"`
	Updated     time.Time      `json:"updated_at"`
	Params      map[string]any `json:"litellm_params"`
	Info        map[string]any `json:"prompt_info"`
}

func (record promptRecord) public() promptRecord {
	record.Params = maps.Clone(record.Params)
	delete(record.Params, "api_key")
	delete(record.Params, "langfuse_secret")
	delete(record.Params, "langfuse_secret_key")
	for _, name := range []string{"gitlab_config", "bitbucket_config"} {
		if config, ok := record.Params[name].(map[string]any); ok {
			config = maps.Clone(config)
			delete(config, "access_token")
			record.Params[name] = config
		}
	}
	delete(record.Params, "provider_specific_query_params")
	if address, ok := record.Params["api_base"].(string); ok {
		if clean, _, found := strings.Cut(address, "?"); found {
			record.Params["api_base"] = clean
		}
	}
	return record
}

func WithPromptManagement(next http.Handler, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+promptTable+` (
		id TEXT PRIMARY KEY, prompt_id TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1,
		environment TEXT NOT NULL DEFAULT 'development', created_by TEXT, litellm_params JSONB NOT NULL,
		prompt_info JSONB, created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		UNIQUE(prompt_id,version,environment))`); err != nil {
		return nil, errors.New("could not initialize prompt registry")
	}
	api := promptAPI{db, keys}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /prompts/list", api.list)
	mux.HandleFunc("GET /prompts/{prompt_id}/versions", api.list)
	mux.HandleFunc("GET /prompts/{prompt_id}", api.info)
	mux.HandleFunc("GET /prompts/{prompt_id}/info", api.info)
	mux.HandleFunc("POST /prompts", api.write)
	mux.HandleFunc("POST /prompts/test", func(w http.ResponseWriter, r *http.Request) { api.testPrompt(w, r, next) })
	mux.HandleFunc("POST /utils/dotprompt_json_converter", api.convertDotprompt)
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(method+" /prompts/{prompt_id}", api.write)
	}
	mux.Handle("/", next)
	return mux, nil
}

func promptIDVersion(id string) (string, int) {
	for _, separator := range []string{".v", "_v"} {
		if pos := strings.LastIndex(id, separator); pos > 0 {
			if version, err := strconv.Atoi(id[pos+len(separator):]); err == nil && version > 0 {
				return id[:pos], version
			}
		}
	}
	return id, 0
}

const promptColumns = `prompt_id,version,environment,created_by,created_at,updated_at,litellm_params,prompt_info`

func scanPrompt(row interface{ Scan(...any) error }) (promptRecord, error) {
	var record promptRecord
	var params, info []byte
	if err := row.Scan(&record.PromptID, &record.Version, &record.Environment, &record.CreatedBy, &record.Created, &record.Updated, &params, &info); err != nil {
		return record, err
	}
	for _, field := range []struct {
		raw    []byte
		target *map[string]any
	}{{params, &record.Params}, {info, &record.Info}} {
		if len(field.raw) == 0 {
			continue
		}
		raw := field.raw
		if raw[0] == '"' {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return record, err
			}
			raw = []byte(value)
		}
		if err := json.Unmarshal(raw, field.target); err != nil {
			return record, err
		}
	}
	return record, nil
}

func (api promptAPI) scope(r *http.Request) (bool, []string, bool) {
	caller, ok := api.keys.identifyResourceCaller(r)
	if !ok || caller.admin {
		return caller.admin, nil, ok
	}
	token := runtime.ProxyCredential(r)
	api.keys.mu.RLock()
	defer api.keys.mu.RUnlock()
	key := api.keys.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key = api.keys.state.Keys[key.RotationTarget]
	}
	var ids []string
	values, _ := key.Metadata["prompts"].([]any)
	for _, value := range values {
		if id, ok := value.(string); ok {
			base, version := promptIDVersion(id)
			if version > 0 {
				base += ".v" + strconv.Itoa(version)
			}
			ids = append(ids, base)
		}
	}
	return false, ids, true
}

func (api promptAPI) list(w http.ResponseWriter, r *http.Request) {
	admin, scope, ok := api.scope(r)
	if !ok {
		modelError(w, 401, "Proxy token required")
		return
	}
	id, _ := promptIDVersion(r.PathValue("prompt_id"))
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	clause := ` WHERE ($1 OR prompt_id=ANY($2::text[]) OR (prompt_id || '.v' || version)=ANY($2::text[])) AND ($3='' OR environment=$3) AND ($4='' OR prompt_id=$4)`
	query := `SELECT ` + promptColumns + ` FROM ` + promptTable + clause + ` ORDER BY version DESC,environment,id`
	if id == "" {
		query = `SELECT DISTINCT ON (prompt_id) ` + promptColumns + ` FROM ` + promptTable + clause + ` ORDER BY prompt_id, version DESC, environment, id`
	}
	rows, err := api.db.QueryContext(ctx, query, admin, pq.Array(scope), r.URL.Query().Get("environment"), id)
	if err != nil {
		modelError(w, 500, "Could not list prompts")
		return
	}
	defer rows.Close()
	data := make([]promptRecord, 0)
	for rows.Next() {
		p, err := scanPrompt(rows)
		if err != nil {
			modelError(w, 500, "Could not read prompt")
			return
		}
		data = append(data, p.public())
	}
	if rows.Err() != nil {
		modelError(w, 500, "Could not list prompts")
		return
	}
	modelJSON(w, 200, map[string]any{"prompts": data})
}

func (api promptAPI) info(w http.ResponseWriter, r *http.Request) {
	admin, scope, ok := api.scope(r)
	if !ok {
		modelError(w, 401, "Proxy token required")
		return
	}
	id, version := promptIDVersion(r.PathValue("prompt_id"))

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	record, err := scanPrompt(api.db.QueryRowContext(ctx, `SELECT `+promptColumns+` FROM `+promptTable+`
		WHERE prompt_id=$1 AND ($2='' OR environment=$2) AND ($3=0 OR version=$3) AND ($4 OR prompt_id=ANY($5::text[]) OR (prompt_id || '.v' || version)=ANY($5::text[]))
		ORDER BY version DESC, (environment='development') DESC, environment LIMIT 1`, id, r.URL.Query().Get("environment"), version, admin, pq.Array(scope)))
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, 400, "Prompt not found")
		return
	}
	if err != nil {
		modelError(w, 500, "Could not read prompt")
		return
	}
	var environments []string
	err = api.db.QueryRowContext(ctx, `SELECT ARRAY(SELECT DISTINCT environment FROM `+promptTable+` WHERE prompt_id=$1 ORDER BY environment)`, id).Scan(pq.Array(&environments))
	if err != nil {
		modelError(w, 500, "Could not read prompt environments")
		return
	}
	var template any
	if items, ok := record.Params["prompt_data"].(map[string]any); ok {
		templateID, _ := record.Params["prompt_id"].(string)
		if templateID == "" {
			templateID = id
		}
		if value, ok := items[templateID].(map[string]any); ok {
			template = map[string]any{"litellm_prompt_id": templateID, "content": value["content"], "metadata": value["metadata"]}
		}
	}
	modelJSON(w, 200, map[string]any{"prompt_spec": record.public(), "raw_prompt_template": template, "environments": environments})
}
func (api promptAPI) write(w http.ResponseWriter, r *http.Request) {
	caller, ok := api.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	if !caller.admin {
		modelError(w, http.StatusForbidden, "Only proxy administrators can manage prompts")
		return
	}

	id, version := promptIDVersion(r.PathValue("prompt_id"))
	environment := r.URL.Query().Get("environment")
	var input struct {
		PromptID string         `json:"prompt_id"`
		Params   map[string]any `json:"litellm_params"`
		Info     map[string]any `json:"prompt_info"`
	}
	if r.Method != http.MethodDelete {
		if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil ||
			(r.Method != http.MethodPatch && input.Params == nil) ||
			(r.Method == http.MethodPatch && input.Params == nil && input.Info == nil) {
			modelError(w, http.StatusBadRequest, "Invalid prompt configuration")
			return
		}
		if r.Method == http.MethodPost {
			id = input.PromptID
		}
		if r.Method != http.MethodPatch {
			environment, _ = input.Info["environment"].(string)
		}
	}
	if !validVectorIndexName(id) || (environment != "" && !validVectorIndexName(environment)) {
		modelError(w, http.StatusBadRequest, "Invalid prompt ID or environment")
		return
	}
	if environment == "" && r.Method != http.MethodDelete {
		environment = "development"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := api.db.BeginTx(ctx, nil)
	if err != nil {
		modelError(w, http.StatusServiceUnavailable, "Could not start prompt update")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "minato:prompt:"+id); err != nil {
		modelError(w, http.StatusServiceUnavailable, "Could not lock prompt versions")
		return
	}

	if r.Method == http.MethodDelete {
		result, err := tx.ExecContext(ctx, `DELETE FROM `+promptTable+` WHERE prompt_id=$1 AND ($2='' OR environment=$2)`, id, environment)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not delete prompt")
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			modelError(w, http.StatusNotFound, "Prompt not found")
			return
		}
		if tx.Commit() != nil {
			modelError(w, http.StatusServiceUnavailable, "Could not commit prompt deletion")
			return
		}
		message := "Prompt " + id + " deleted successfully"
		if environment != "" {
			message += " from " + environment
		}
		modelJSON(w, http.StatusOK, map[string]string{"message": message})
		return
	}
	if r.Method != http.MethodPatch && input.Info == nil {
		input.Info = map[string]any{"prompt_type": "db"}
	}
	var params, info any
	if input.Params != nil {
		raw, _ := json.Marshal(input.Params)
		params = string(raw)
	}
	if input.Info != nil {
		raw, _ := json.Marshal(input.Info)
		info = string(raw)
	}
	var row *sql.Row
	if r.Method == http.MethodPatch {
		row = tx.QueryRowContext(ctx, `UPDATE `+promptTable+` SET litellm_params=COALESCE($4,litellm_params),
			prompt_info=COALESCE($5,prompt_info), updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
			WHERE id=(SELECT id FROM `+promptTable+` WHERE prompt_id=$1 AND environment=$2 AND ($3=0 OR version=$3)
			ORDER BY version DESC LIMIT 1) RETURNING `+promptColumns, id, environment, version, params, info)
	} else {
		row = tx.QueryRowContext(ctx, `INSERT INTO `+promptTable+` (id,prompt_id,version,environment,created_by,litellm_params,prompt_info)
			SELECT $1,$2,(SELECT COALESCE(MAX(version),0)+1 FROM `+promptTable+` WHERE prompt_id=$2 AND environment=$3),
			$3,NULLIF($4,''),$5,$6 WHERE ($7 OR EXISTS(SELECT 1 FROM `+promptTable+` WHERE prompt_id=$2))
			RETURNING `+promptColumns, newEntityID(), id, environment, caller.user, params, info, r.Method == http.MethodPost)
	}
	record, err := scanPrompt(row)
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusNotFound, "Prompt not found")
		return
	}
	if err != nil || tx.Commit() != nil {
		modelError(w, http.StatusInternalServerError, "Could not store prompt")
		return
	}
	record.PromptID += ".v" + strconv.Itoa(record.Version)
	modelJSON(w, http.StatusOK, record.public())
}
