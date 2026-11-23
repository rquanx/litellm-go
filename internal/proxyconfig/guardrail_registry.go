package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type guardrailRegistry struct {
	db     *sql.DB
	keys   *KeyRegistry
	server *runtime.Server
	static []runtime.GenericGuardrail
	mu     sync.Mutex
	rows   map[string]guardrailRecord
}

type guardrailRecord struct {
	ID   string `json:"guardrail_id"`
	Spec genericGuardrailSpec
	Info map[string]any
}

const guardrailTable = `"LiteLLM_GuardrailsTable"`

func WithGuardrailRegistry(next http.Handler, server *runtime.Server, keys *KeyRegistry, db *sql.DB, static []runtime.GenericGuardrail) (http.Handler, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+guardrailTable+` (
		guardrail_id TEXT PRIMARY KEY, guardrail_name TEXT NOT NULL UNIQUE, litellm_params JSONB NOT NULL, guardrail_info JSONB,
		team_id TEXT, created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		status TEXT NOT NULL DEFAULT 'active', submitted_at TIMESTAMP(3), reviewed_at TIMESTAMP(3)
	)`); err != nil {
		return nil, errors.New("could not initialize guardrails registry")
	}
	registry := &guardrailRegistry{db: db, keys: keys, server: server, static: static, rows: make(map[string]guardrailRecord)}
	records, err := db.QueryContext(ctx, `SELECT guardrail_id,guardrail_name,litellm_params,guardrail_info FROM `+guardrailTable+`
		WHERE status='active' AND team_id IS NULL`)
	if err != nil {
		return nil, err
	}
	defer records.Close()
	for records.Next() {
		var row guardrailRecord
		var params, info []byte
		if err := records.Scan(&row.ID, &row.Spec.Name, &params, &info); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(params, &row.Spec.Params)
		if !supportedGuardrailProvider(row.Spec.Params.Provider) {
			continue
		}
		_ = json.Unmarshal(info, &row.Info)
		registry.rows[row.ID] = row
	}
	if err := records.Err(); err != nil {
		return nil, err
	}
	guards, err := registry.compile(registry.rows)
	if err != nil {
		return nil, err
	}
	server.ReplaceGuardrails(guards)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /guardrails/list", registry.read)
	mux.HandleFunc("GET /v2/guardrails/list", registry.read)
	mux.HandleFunc("GET /guardrails/{guardrail_id}", registry.read)
	mux.HandleFunc("GET /guardrails/{guardrail_id}/info", registry.read)
	mux.HandleFunc("POST /guardrails", registry.write)
	mux.HandleFunc("PUT /guardrails/{guardrail_id}", registry.write)
	mux.HandleFunc("PATCH /guardrails/{guardrail_id}", registry.write)
	mux.HandleFunc("DELETE /guardrails/{guardrail_id}", registry.write)
	mux.Handle("/", next)
	return mux, nil
}

func (registry *guardrailRegistry) compile(rows map[string]guardrailRecord) ([]runtime.GenericGuardrail, error) {
	guards := append([]runtime.GenericGuardrail{}, registry.static...)
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	specs := make([]genericGuardrailSpec, 0, len(rows))
	for _, id := range ids {
		row := rows[id]
		for _, guard := range registry.static {
			if row.Spec.Name == guard.Name {
				return nil, errors.New("Guardrail name already configured")
			}
		}
		specs = append(specs, row.Spec)
	}
	compiled, err := prepareGenericGuardrails(specs)
	return append(guards, compiled...), err
}

func guardrailListEntry(id string, guard runtime.GenericGuardrail, info map[string]any) map[string]any {
	mode := "pre_call"
	if guard.Post {
		mode = "post_call"
	}
	if info == nil {
		info = map[string]any{}
	}
	return map[string]any{"guardrail_id": id, "guardrail_name": guard.Name, "guardrail_info": info,
		"litellm_params": map[string]any{"guardrail": guard.Provider, "mode": mode, "default_on": guard.Default}}
}

func (registry *guardrailRegistry) read(w http.ResponseWriter, r *http.Request) {
	if _, ok := registry.keys.identifyResourceCaller(r); !ok {
		modelError(w, 401, "Proxy token required")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	id := r.PathValue("guardrail_id")
	result := make([]map[string]any, 0, len(registry.rows)+len(registry.static))
	for _, guard := range registry.static {
		item := guardrailListEntry(guard.Name, guard, nil)
		if id == guard.Name {
			modelJSON(w, 200, item)
			return
		}
		result = append(result, item)
	}
	for key, row := range registry.rows {
		item := guardrailListEntry(key, runtime.GenericGuardrail{Name: row.Spec.Name, Post: row.Spec.Params.Mode == "post_call", Default: row.Spec.Params.Default, Provider: row.Spec.Params.Provider}, row.Info)
		if id == key || id == row.Spec.Name {
			modelJSON(w, 200, item)
			return
		}
		result = append(result, item)
	}
	if id != "" {
		modelError(w, 404, "Guardrail not found")
		return
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i]["guardrail_name"].(string) < result[j]["guardrail_name"].(string)
	})
	modelJSON(w, 200, map[string]any{"guardrails": result})
}
func (registry *guardrailRegistry) write(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.requireMaster(w, r) {
		return
	}
	id := r.PathValue("guardrail_id")
	type definition struct {
		genericGuardrailSpec
		Info map[string]any `json:"guardrail_info"`
	}
	var request struct {
		Guardrail definition `json:"guardrail"`
	}
	var patch json.RawMessage
	var target any = &request
	if r.Method == http.MethodPatch {
		target = &patch
	}
	if r.Method != http.MethodDelete && decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), target) != nil {
		modelError(w, 400, "Invalid guardrail configuration")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	next := make(map[string]guardrailRecord, len(registry.rows)+1)
	for key, row := range registry.rows {
		next[key] = row
	}
	if r.Method == http.MethodPost {
		id = newEntityID()
	} else if _, ok := next[id]; !ok {
		modelError(w, 404, "Guardrail not found")
		return
	}
	if r.Method == http.MethodPatch {
		row := next[id]
		update := definition{genericGuardrailSpec: row.Spec, Info: nil}
		if decodeModelJSON(strings.NewReader(string(patch)), &update) != nil {
			modelError(w, 400, "Invalid guardrail patch")
			return
		}
		if update.Info == nil {
			update.Info = row.Info
		}
		request.Guardrail = update
	}
	if r.Method == http.MethodDelete {
		delete(next, id)
	} else {
		next[id] = guardrailRecord{ID: id, Spec: request.Guardrail.genericGuardrailSpec, Info: request.Guardrail.Info}
	}
	guards, err := registry.compile(next)
	if err != nil {
		modelError(w, 400, "Invalid or duplicate guardrail")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if r.Method == http.MethodDelete {
		_, err = registry.db.ExecContext(ctx, `DELETE FROM `+guardrailTable+` WHERE guardrail_id=$1`, id)
	} else {
		row := next[id]
		raw, _ := json.Marshal(row.Spec.Params)
		info, _ := json.Marshal(row.Info)
		_, err = registry.db.ExecContext(ctx, `INSERT INTO `+guardrailTable+`(guardrail_id,guardrail_name,litellm_params,guardrail_info)
			VALUES($1,$2,$3,$4) ON CONFLICT(guardrail_id) DO UPDATE SET guardrail_name=EXCLUDED.guardrail_name,
			litellm_params=EXCLUDED.litellm_params,guardrail_info=EXCLUDED.guardrail_info,updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'`,
			id, row.Spec.Name, string(raw), string(info))
	}
	if err != nil {
		modelError(w, 500, "Could not store guardrail")
		return
	}
	registry.rows = next
	registry.server.ReplaceGuardrails(guards)
	if r.Method == http.MethodDelete {
		modelJSON(w, 200, map[string]string{"message": "Guardrail deleted successfully"})
		return
	}
	row := next[id]
	modelJSON(w, 200, guardrailListEntry(id, runtime.GenericGuardrail{Name: row.Spec.Name, Post: row.Spec.Params.Mode == "post_call", Default: row.Spec.Params.Default, Provider: row.Spec.Params.Provider}, row.Info))
}
