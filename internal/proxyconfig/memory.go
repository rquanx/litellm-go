package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type memoryAPI struct {
	db   *sql.DB
	keys *KeyRegistry
}

type memoryCaller struct {
	admin, teamAdmin bool
	user, team       string
}

type memoryEntry struct {
	ID        string          `json:"memory_id"`
	Key       string          `json:"key"`
	Value     string          `json:"value"`
	Metadata  json.RawMessage `json:"metadata"`
	User      *string         `json:"user_id"`
	Team      *string         `json:"team_id"`
	Created   time.Time       `json:"created_at"`
	By        *string         `json:"created_by"`
	Updated   time.Time       `json:"updated_at"`
	UpdatedBy *string         `json:"updated_by"`
}

type memoryInput struct {
	Key      string          `json:"key"`
	Value    *string         `json:"value"`
	Metadata json.RawMessage `json:"metadata"`
	User     *string         `json:"user_id"`
	Team     *string         `json:"team_id"`
}

const memoryTable = `"LiteLLM_MemoryTable"`
const memoryColumns = `memory_id,key,value,COALESCE(metadata,'null'::jsonb),user_id,team_id,created_at,created_by,updated_at,updated_by`
const memoryVisible = `($1 OR (user_id IS NOT NULL AND user_id = NULLIF($2,'')) OR (team_id IS NOT NULL AND team_id = NULLIF($3,'')))`

func WithMemoryManagement(next http.Handler, db *sql.DB, keys *KeyRegistry) (http.Handler, error) {
	api := &memoryAPI{db: db, keys: keys}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+memoryTable+` (
		memory_id TEXT PRIMARY KEY, key TEXT NOT NULL UNIQUE, value TEXT NOT NULL, metadata JSONB,
		user_id TEXT, team_id TEXT, created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		created_by TEXT, updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), updated_by TEXT);
		CREATE INDEX IF NOT EXISTS "LiteLLM_MemoryTable_user_id_idx" ON `+memoryTable+`(user_id);
		CREATE INDEX IF NOT EXISTS "LiteLLM_MemoryTable_team_id_idx" ON `+memoryTable+`(team_id);`); err != nil {
		return nil, errors.New("could not initialize memory storage")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/memory", api.serve)
	mux.HandleFunc("POST /v1/memory", api.serve)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		mux.HandleFunc(method+" /v1/memory/{key...}", api.serve)
	}
	mux.Handle("/", next)
	return mux, nil
}

func (api *memoryAPI) identify(r *http.Request) (memoryCaller, bool) {
	return api.keys.identifyResourceCaller(r)
}

func (keys *KeyRegistry) identifyResourceCaller(r *http.Request) (memoryCaller, bool) {
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	return keys.identifyResourceCallerLocked(r)
}

func (keys *KeyRegistry) identifyResourceCallerLocked(r *http.Request) (memoryCaller, bool) {
	token := runtime.ProxyCredential(r)
	if keys.master != "" && token == keys.master {
		return memoryCaller{admin: true}, true
	}
	if _, _, ok := keys.authorizeLocked(token); !ok {
		if user,ok:=keys.ssoViewerLocked(r,token);ok {
			return memoryCaller{user:user},true
		}
		return memoryCaller{}, false
	}
	key, exists := keys.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key, exists = keys.state.Keys[key.RotationTarget]
	}
	if !exists || key.Blocked || (key.Expires != nil && !time.Now().Before(*key.Expires)) {
		return memoryCaller{}, false
	}
	teamID := key.TeamID
	if teamID == "" && key.ProjectID != "" {
		teamID = keys.state.Projects[key.ProjectID].TeamID
	}
	caller := memoryCaller{user: key.UserID, team: teamID}
	if teamID != "" && key.UserID != "" {
		team := keys.state.Teams[teamID]
		if index := memberIndex(team, key.UserID); index >= 0 {
			caller.teamAdmin = team.Members[index].Role == "admin"
		}
		if !caller.teamAdmin && team.OrgID != "" {
			org := keys.state.Organizations[team.OrgID]
			if index := organizationMemberIndex(org, key.UserID); index >= 0 {
				caller.teamAdmin = org.Members[index].Role == "org_admin"
			}
		}
	}
	return caller, true
}

func (entry *memoryEntry) scan(row interface{ Scan(...any) error }) error {
	return row.Scan(&entry.ID, &entry.Key, &entry.Value, &entry.Metadata, &entry.User, &entry.Team,
		&entry.Created, &entry.By, &entry.Updated, &entry.UpdatedBy)
}

func memoryError(w http.ResponseWriter, status int, message string) {
	modelJSON(w, status, map[string]string{"detail": message})
}

func (api *memoryAPI) serve(w http.ResponseWriter, r *http.Request) {
	caller, ok := api.identify(r)
	if !ok {
		memoryError(w, 401, "Valid proxy credentials are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	key := r.PathValue("key")
	if r.Method == http.MethodGet {
		if key == "" && r.URL.Path == "/v1/memory" {
			api.list(w, r.WithContext(ctx), caller)
			return
		}
		var entry memoryEntry
		err := entry.scan(api.db.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM `+memoryTable+
			` WHERE `+memoryVisible+` AND key=$4`, caller.admin, caller.user, caller.team, key))
		if errors.Is(err, sql.ErrNoRows) {
			memoryError(w, 404, "Memory entry not found")
			return
		}
		if err != nil {
			memoryError(w, 500, "Could not read memory")
			return
		}
		modelJSON(w, 200, entry)
		return
	}
	var input memoryInput
	if r.Method != http.MethodDelete && decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil {
		memoryError(w, 400, "Invalid memory entry")
		return
	}
	if r.Method == http.MethodPost {
		key = input.Key
	}
	if strings.TrimSpace(key) == "" || len(key) > 1024 || strings.ContainsRune(key, 0) {
		memoryError(w, 400, "A valid memory key is required")
		return
	}
	if r.Method == http.MethodPost && input.Value == nil {
		memoryError(w, 400, "A memory value is required")
		return
	}
	api.mutate(w, ctx, r.Method, key, caller, input)
}

func (api *memoryAPI) list(w http.ResponseWriter, r *http.Request, caller memoryCaller) {
	query := r.URL.Query()
	page, size := 1, 50
	var err error
	if query.Has("page") {
		page, err = strconv.Atoi(query.Get("page"))
	}
	if err != nil || page < 1 || page > 1000000 {
		memoryError(w, 400, "Invalid page")
		return
	}
	if query.Has("page_size") {
		size, err = strconv.Atoi(query.Get("page_size"))
	}
	if err != nil || size < 1 || size > 500 {
		memoryError(w, 400, "Invalid page_size")
		return
	}
	where := ` FROM ` + memoryTable + ` WHERE ` + memoryVisible
	args := []any{caller.admin, caller.user, caller.team}
	if query.Has("key_prefix") {
		where += ` AND starts_with(key,$4)`
		args = append(args, query.Get("key_prefix"))
	} else if query.Has("key") {
		where += ` AND key=$4`
		args = append(args, query.Get("key"))
	}
	tx, err := api.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		memoryError(w, 500, "Could not read memories")
		return
	}
	defer tx.Rollback()
	var count int64
	if tx.QueryRowContext(r.Context(), `SELECT count(*)`+where, args...).Scan(&count) != nil {
		memoryError(w, 500, "Could not count memories")
		return
	}
	limit, offset := "$"+strconv.Itoa(len(args)+1), "$"+strconv.Itoa(len(args)+2)
	rows, err := tx.QueryContext(r.Context(), `SELECT `+memoryColumns+where+` ORDER BY updated_at DESC, key LIMIT `+limit+
		` OFFSET `+offset, append(args, size, (page-1)*size)...)
	if err != nil {
		memoryError(w, 500, "Could not read memories")
		return
	}
	defer rows.Close()
	entries := make([]memoryEntry, 0, size)
	for rows.Next() {
		var item memoryEntry
		if item.scan(rows) != nil {
			memoryError(w, 500, "Could not decode memories")
			return
		}
		entries = append(entries, item)
	}
	if rows.Err() != nil {
		memoryError(w, 500, "Could not read memories")
		return
	}
	modelJSON(w, 200, map[string]any{"memories": entries, "total": count})
}

func (api *memoryAPI) mutate(w http.ResponseWriter, ctx context.Context, method, key string, caller memoryCaller, input memoryInput) {
	tx, err := api.db.BeginTx(ctx, nil)
	if err != nil {
		memoryError(w, 500, "Could not open memory transaction")
		return
	}
	defer tx.Rollback()
	var entry memoryEntry
	if method != http.MethodPost {
		err = entry.scan(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM `+memoryTable+
			` WHERE `+memoryVisible+` AND key=$4 FOR UPDATE`, caller.admin, caller.user, caller.team, key))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			memoryError(w, 500, "Could not read memory")
			return
		}
		if entry.ID != "" && !caller.admin && !(entry.User != nil && *entry.User == caller.user) &&
			!(entry.User == nil && entry.Team != nil && caller.team == *entry.Team && caller.teamAdmin) {
			memoryError(w, 403, "Write access to this memory entry is denied")
			return
		}
	}
	switch {
	case method == http.MethodDelete:
		if entry.ID == "" {
			memoryError(w, 404, "Memory entry not found")
			return
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM `+memoryTable+` WHERE memory_id=$1`, entry.ID)
	case entry.ID == "":
		if input.Value == nil {
			memoryError(w, 400, "A memory value is required")
			return
		}
		var user, team any
		if caller.admin {
			user, team = input.User, input.Team
		} else {
			if caller.user == "" && !caller.teamAdmin {
				memoryError(w, 403, "A user identity or team administrator is required")
				return
			}
			if caller.user != "" {
				user = caller.user
			}
			if caller.team != "" {
				team = caller.team
			}
		}
		err = entry.scan(tx.QueryRowContext(ctx, `INSERT INTO `+memoryTable+`
			(memory_id,key,value,metadata,user_id,team_id,created_by,updated_by)
			VALUES ($1,$2,$3,$4::jsonb,$5,$6,NULLIF($7,''),NULLIF($7,''))
			ON CONFLICT (key) DO NOTHING RETURNING `+memoryColumns,
			newEntityID(), key, input.Value, memoryMetadata(input.Metadata), user, team, caller.user))
		if errors.Is(err, sql.ErrNoRows) {
			memoryError(w, 409, "Memory key already exists")
			return
		}
	default:
		err = entry.scan(tx.QueryRowContext(ctx, `UPDATE `+memoryTable+` SET value=COALESCE($2,value),
			metadata=CASE WHEN $3 THEN $4::jsonb ELSE metadata END,
			updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC',updated_by=NULLIF($5,'') WHERE memory_id=$1 RETURNING `+memoryColumns,
			entry.ID, input.Value, input.Metadata != nil, memoryMetadata(input.Metadata), caller.user))
	}
	if err != nil || tx.Commit() != nil {
		memoryError(w, 500, "Could not save memory entry")
		return
	}
	if method == http.MethodDelete {
		modelJSON(w, 200, map[string]any{"key": key, "deleted": true})
		return
	}
	modelJSON(w, 200, entry)
}

func memoryMetadata(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}
