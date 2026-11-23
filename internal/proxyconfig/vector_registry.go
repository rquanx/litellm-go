package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

const vectorRegistryTable = `"LiteLLM_ManagedVectorStoresTable"`
const vectorRegistryScope = `($1 OR team_id=NULLIF($3,'') OR (team_id IS NULL AND user_id=NULLIF($2,'')))`

type vectorRegistry struct {
	db   *sql.DB
	keys *KeyRegistry
}

func WithVectorRegistry(next http.Handler, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+vectorRegistryTable+` (
		vector_store_id TEXT PRIMARY KEY, custom_llm_provider TEXT NOT NULL, vector_store_name TEXT,
		vector_store_description TEXT, vector_store_metadata JSONB, litellm_params JSONB, litellm_credential_name TEXT,
		team_id TEXT, user_id TEXT, created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'));
		CREATE INDEX IF NOT EXISTS "LiteLLM_ManagedVectorStoresTable_team_id_idx" ON `+vectorRegistryTable+`(team_id);
		CREATE INDEX IF NOT EXISTS "LiteLLM_ManagedVectorStoresTable_user_id_idx" ON `+vectorRegistryTable+`(user_id);
		CREATE TABLE IF NOT EXISTS "LiteLLM_ManagedVectorStoreIndexTable" (
			id TEXT PRIMARY KEY, index_name TEXT NOT NULL UNIQUE, litellm_params JSONB NOT NULL,
			index_info JSONB, created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
			created_by TEXT, updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), updated_by TEXT
		);`)
	if err != nil {
		return nil, errors.New("could not initialize managed vector stores")
	}
	api := &vectorRegistry{db: db, keys: keys}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/indexes", api.createIndex)
	for _, prefix := range []string{"/vector_store/", "/v1/vector_store/"} {
		mux.HandleFunc("GET "+prefix+"list", api.serve)
		for _, action := range []string{"new", "info", "update", "delete"} {
			mux.HandleFunc("POST "+prefix+action, api.serve)
		}
	}
	mux.Handle("/", next)
	return mux, nil
}

func safeVectorParams(value any) map[string]any {
	params, _ := value.(map[string]any)
	if encoded, ok := value.(string); ok {
		_ = json.Unmarshal([]byte(encoded), &params)
	}
	safe := make(map[string]any)
	for _, name := range []string{"model", "embedding_model", "litellm_embedding_model", "vector_bucket_name", "aws_region_name", "vertex_project", "vertex_location", "vertex_collection_id", "milvus_db_name", "milvus_text_field"} {
		if field, ok := params[name].(string); ok {
			safe[name] = field
		}
	}
	if names, ok := params["milvus_partition_names"].([]any); ok {
		valid := true
		for _, name := range names {
			if _, ok := name.(string); !ok {
				valid = false
				break
			}
		}
		if valid {
			safe["milvus_partition_names"] = names
		}
	}
	return safe
}

func readVectorRegistryRow(row interface{ Scan(...any) error }) (map[string]any, error) {
	var data []byte
	if err := row.Scan(&data); err != nil {
		return nil, err
	}
	var output map[string]any
	if err := json.Unmarshal(data, &output); err != nil {
		return nil, err
	}
	output["litellm_params"] = safeVectorParams(output["litellm_params"])
	if raw, ok := output["vector_store_metadata"].(string); ok {
		var metadata map[string]any
		if json.Unmarshal([]byte(raw), &metadata) == nil {
			output["vector_store_metadata"] = metadata
		}
	}
	return output, nil
}

func (api *vectorRegistry) serve(w http.ResponseWriter, r *http.Request) {
	caller, ok := api.keys.identifyResourceCaller(r)
	if !ok {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if r.Method == http.MethodGet {
		api.list(w, r.WithContext(ctx), caller)
		return
	}
	var input struct {
		ID          string         `json:"vector_store_id"`
		Provider    *string        `json:"custom_llm_provider"`
		Name        *string        `json:"vector_store_name"`
		Description *string        `json:"vector_store_description"`
		Metadata    map[string]any `json:"vector_store_metadata"`
		Params      map[string]any `json:"litellm_params"`
		Credential  *string        `json:"litellm_credential_name"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || strings.TrimSpace(input.ID) == "" || len(input.ID) > 8192 {
		modelError(w, http.StatusBadRequest, "A vector_store_id is required")
		return
	}
	_, action, _ := strings.Cut(r.URL.Path, "/vector_store/")
	if (action == "new" && input.Provider == nil) || (input.Provider != nil && strings.TrimSpace(*input.Provider) == "") {
		modelError(w, http.StatusBadRequest, "custom_llm_provider is required")
		return
	}
	var metadata, params any
	if input.Metadata != nil {
		data, _ := json.Marshal(input.Metadata)
		metadata = string(data)
	}
	if input.Params != nil {
		data, _ := json.Marshal(safeVectorParams(input.Params))
		params = string(data)
	}
	args := []any{caller.admin, caller.user, caller.team, input.ID}
	var row *sql.Row
	var message string
	switch action {
	case "new":
		if !caller.admin && caller.user == "" && caller.team == "" {
			modelError(w, http.StatusForbidden, "Vector store ownership requires a user or team")
			return
		}
		row = api.db.QueryRowContext(ctx, `INSERT INTO `+vectorRegistryTable+` AS entry
			(vector_store_id,custom_llm_provider,vector_store_name,vector_store_description,vector_store_metadata,litellm_params,litellm_credential_name,team_id,user_id,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),CURRENT_TIMESTAMP AT TIME ZONE 'UTC') RETURNING to_jsonb(entry)`,
			input.ID, input.Provider, input.Name, input.Description, metadata, params, input.Credential, caller.team, caller.user)
		message = "created"
	case "info":
		row = api.db.QueryRowContext(ctx, `SELECT to_jsonb(entry) FROM `+vectorRegistryTable+` AS entry WHERE `+vectorRegistryScope+` AND vector_store_id=$4`, args...)
	case "update":
		args = append(args, input.Provider, input.Name, input.Description, metadata, params, input.Credential)
		row = api.db.QueryRowContext(ctx, `UPDATE `+vectorRegistryTable+` AS entry SET custom_llm_provider=COALESCE($5,custom_llm_provider),
			vector_store_name=COALESCE($6,vector_store_name),vector_store_description=COALESCE($7,vector_store_description),
			vector_store_metadata=COALESCE($8,vector_store_metadata),litellm_params=COALESCE($9,litellm_params),
			litellm_credential_name=COALESCE($10,litellm_credential_name),updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
			WHERE `+vectorRegistryScope+` AND vector_store_id=$4 RETURNING to_jsonb(entry)`, args...)
		message = "updated"
	case "delete":
		row = api.db.QueryRowContext(ctx, `DELETE FROM `+vectorRegistryTable+` AS entry WHERE `+vectorRegistryScope+` AND vector_store_id=$4 RETURNING to_jsonb(entry)`, args...)
		message = "deleted"
	default:
		http.NotFound(w, r)
		return
	}
	record, err := readVectorRegistryRow(row)
	if err != nil {
		var pg *pq.Error
		switch {
		case errors.Is(err, sql.ErrNoRows):
			modelError(w, http.StatusNotFound, "Vector store not found")
		case errors.As(err, &pg) && pg.Code == "23505":
			modelError(w, http.StatusConflict, "Vector store already exists")
		default:
			modelError(w, http.StatusInternalServerError, "Could not manage vector store")
		}
		return
	}
	response := make(map[string]any)
	if message != "" {
		response["status"] = "success"
		response["message"] = fmt.Sprintf("Vector store %s %s successfully", input.ID, message)
	}
	if action != "delete" {
		response["vector_store"] = record
	}
	modelJSON(w, http.StatusOK, response)
}

func (api *vectorRegistry) list(w http.ResponseWriter, r *http.Request, caller memoryCaller) {
	page, size := 1, 100
	query := r.URL.Query()
	for name, option := range map[string]*int{"page": &page, "page_size": &size} {
		if value := query.Get(name); value != "" {
			number, err := strconv.Atoi(value)
			if err != nil || number < 1 || number > 1000000 {
				modelError(w, http.StatusBadRequest, "Invalid vector store pagination")
				return
			}
			*option = number
		}
	}
	tx, err := api.db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not list managed vector stores")
		return
	}
	defer tx.Rollback()
	args := []any{caller.admin, caller.user, caller.team}
	var count int
	if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM `+vectorRegistryTable+` WHERE `+vectorRegistryScope, args...).Scan(&count); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not count managed vector stores")
		return
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT to_jsonb(entry) FROM `+vectorRegistryTable+` AS entry WHERE `+
		vectorRegistryScope+` ORDER BY vector_store_id LIMIT $4 OFFSET $5`, append(args, size, int64(page-1)*int64(size))...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not list managed vector stores")
		return
	}
	defer rows.Close()
	records := []map[string]any{}
	for rows.Next() {
		record, err := readVectorRegistryRow(rows)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read managed vector store")
			return
		}
		records = append(records, record)
	}
	if rows.Err() != nil || tx.Commit() != nil {
		modelError(w, http.StatusInternalServerError, "Could not list managed vector stores")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"object": "list", "data": records, "total_count": count, "current_page": page, "total_pages": (count + size - 1) / size})
}

func VectorRegistryLookup(db *sql.DB, keys *KeyRegistry) runtime.VectorRegistryLookup {
	return func(r *http.Request, id string) (provider string, params map[string]any, found, allowed bool, err error) {
		caller, ok := keys.identifyResourceCaller(r)
		if !ok {
			return "", nil, false, false, nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		var raw []byte
		err = db.QueryRowContext(ctx, `SELECT COALESCE(`+vectorRegistryScope+`,false),custom_llm_provider,litellm_params
			FROM `+vectorRegistryTable+` WHERE vector_store_id=$4`, caller.admin, caller.user, caller.team, id).Scan(&allowed, &provider, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, false, false, nil
		}
		if err != nil || !allowed {
			return "", nil, err == nil, false, err
		}
		var value any
		_ = json.Unmarshal(raw, &value)
		return provider, safeVectorParams(value), true, true, nil
	}
}
