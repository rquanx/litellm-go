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

type managementAuditEvent struct {
	id, actor, key, table, object, action string
	before, after json.RawMessage
}

type managementAuditRecord struct {
	ID string `json:"id"`
	Updated time.Time `json:"updated_at"`
	Actor string `json:"changed_by"`
	Key string `json:"changed_by_api_key"`
	Action string `json:"action"`
	Table string `json:"table_name"`
	Object string `json:"object_id"`
	Before json.RawMessage `json:"before_value"`
	After json.RawMessage `json:"updated_values"`
}

const auditColumns = "id, updated_at, changed_by, changed_by_api_key, action, table_name, object_id, before_value, updated_values"

func InitManagementAudit(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS "LiteLLM_AuditLog" (
		id TEXT PRIMARY KEY, updated_at TIMESTAMP NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'),
		changed_by TEXT NOT NULL, changed_by_api_key TEXT NOT NULL, action TEXT NOT NULL,
		table_name TEXT NOT NULL, object_id TEXT NOT NULL, before_value JSONB, updated_values JSONB
	);
	CREATE INDEX IF NOT EXISTS minato_management_audit_time ON "LiteLLM_AuditLog" (updated_at DESC, id)`)
	return err
}

func newSettingsAudit(r *http.Request, table, object string, before, after any) *managementAuditEvent {
	oldValue, _ := json.Marshal(before)
	newValue, _ := json.Marshal(after)
	return &managementAuditEvent{id: newEntityID(), actor: "default_user_id",
		key: keyDigest(runtime.ProxyCredential(r)), table: table, object: object, before: oldValue, after: newValue}
}

func (event *managementAuditEvent) insert(ctx context.Context, tx *sql.Tx, created bool) error {
	action := event.action
	if action == "" {
		action = "updated"
		if created {
			action = "created"
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO "LiteLLM_AuditLog" (`+auditColumns+`)
		VALUES ($1, CURRENT_TIMESTAMP AT TIME ZONE 'UTC', $2,$3,$4,$5,$6,$7::jsonb,$8::jsonb)`,
		event.id, event.actor, event.key, action, event.table, event.object, string(event.before), string(event.after))
	return err
}

func scanManagementAudit(row interface{ Scan(...any) error }) (managementAuditRecord, error) {
	var entry managementAuditRecord
	err := row.Scan(&entry.ID, &entry.Updated, &entry.Actor, &entry.Key, &entry.Action, &entry.Table, &entry.Object, &entry.Before, &entry.After)
	return entry, err
}

func WithManagementAudit(next http.Handler, db *sql.DB, keys *KeyRegistry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /audit/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !keys.requireAdminRead(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		record, err := scanManagementAudit(db.QueryRowContext(ctx, `SELECT `+auditColumns+` FROM "LiteLLM_AuditLog" WHERE id=$1`, r.PathValue("id")))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			modelError(w, http.StatusNotFound, "Audit log not found")
		case err != nil:
			modelError(w, http.StatusInternalServerError, "Could not read audit log")
		default:
			modelJSON(w, http.StatusOK, record)
		}
	})
	mux.HandleFunc("GET /audit", func(w http.ResponseWriter, r *http.Request) {
		if keys.requireAdminRead(w, r) {
			listManagementAudit(w, r, db)
		}
	})
	mux.Handle("/", next)
	return mux
}

func listManagementAudit(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	query := r.URL.Query()
	page, pageErr := keyListInt(query, "page", 1)
	size, sizeErr := keyListInt(query, "page_size", 10)
	if pageErr != nil || sizeErr != nil || size > 100 || page-1 > int(^uint(0)>>1)/size {
		modelError(w, http.StatusUnprocessableEntity, "Invalid audit pagination")
		return
	}
	start, startErr := spendTime(query.Get("start_date"), time.Unix(0, 0), false)
	end, endErr := spendTime(query.Get("end_date"), time.Now().Add(time.Minute), true)
	if startErr != nil || endErr != nil || !start.Before(end) {
		modelError(w, http.StatusBadRequest, "Invalid audit date range")
		return
	}
	order := query.Get("sort_by")
	switch order {
	case "":
		order = "updated_at"
	case "updated_at", "action", "table_name", "object_id", "changed_by":
	default:
		modelError(w, http.StatusBadRequest, "Invalid audit sort field")
		return
	}
	direction := strings.ToUpper(query.Get("sort_order"))
	switch direction {
	case "":
		direction = "DESC"
	case "ASC", "DESC":
	default:
		modelError(w, http.StatusBadRequest, "Invalid audit sort direction")
		return
	}
	where := ` FROM "LiteLLM_AuditLog" WHERE updated_at >= ($1::timestamptz AT TIME ZONE 'UTC')
		AND updated_at < ($2::timestamptz AT TIME ZONE 'UTC')`
	args := []any{start, end}
	for _, field := range []string{"changed_by", "changed_by_api_key", "action", "table_name", "object_id"} {
		if value := query.Get(field); value != "" {
			args = append(args, value)
			where += " AND "+field+" = $"+strconv.Itoa(len(args))
		}
	}
	for _, field := range []struct{ param, column string }{{"object_team_id", "team_id"}, {"object_key_hash", "token"}} {
		if value := query.Get(field.param); value != "" {
			args = append(args, value)
			index := strconv.Itoa(len(args))
			where += " AND (strpos(before_value->>'"+field.column+"', $"+index+") > 0 OR strpos(updated_values->>'"+field.column+"', $"+index+") > 0)"
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read audit logs")
		return
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*)"+where, args...).Scan(&count); err != nil {
		modelError(w, http.StatusInternalServerError, "Could not count audit logs")
		return
	}
	limit, offset := strconv.Itoa(len(args)+1), strconv.Itoa(len(args)+2)
	rows, err := tx.QueryContext(ctx, "SELECT "+auditColumns+where+" ORDER BY "+order+" "+direction+", id LIMIT $"+limit+
		" OFFSET $"+offset, append(args, size, (page-1)*size)...)
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read audit logs")
		return
	}
	defer rows.Close()
	records := []managementAuditRecord{}
	for rows.Next() {
		record, err := scanManagementAudit(rows)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not decode audit log")
			return
		}
		records = append(records, record)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not finish reading audit logs")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"audit_logs": records, "total": count, "page": page,
		"page_size": size, "total_pages": (count+size-1)/size})
}


// SetStoreAuditLogs configures management auditing before the registry serves requests.
func (registry *KeyRegistry) SetStoreAuditLogs(enabled bool) {
	registry.storeAuditLogs = enabled
}

func (registry *KeyRegistry) keyAudit(r *http.Request, digest, action string, before, after *virtualKey) *managementAuditEvent {
	if !registry.storeAuditLogs {
		return nil
	}
	snapshot := func(key *virtualKey) any {
		if key == nil {
			return nil
		}
		return map[string]any{"token": digest, "team_id": key.TeamID, "user_id": key.UserID,
			"key_alias": key.Alias, "blocked": key.Blocked, "expires": key.Expires, "models": key.Models,
			"project_id": key.ProjectID, "organization_id": key.OrgID}
	}
	event := newSettingsAudit(r, "LiteLLM_VerificationToken", digest, snapshot(before), snapshot(after))
	event.action = action
	registry.attributeAuditActor(event)
	return event
}

func (registry *KeyRegistry) entityAudit(r *http.Request, table, id, action string, before, after any) *managementAuditEvent {
	if !registry.storeAuditLogs {
		return nil
	}
	event := newSettingsAudit(r, table, id, before, after)
	event.action = action
	registry.attributeAuditActor(event)
	return event
}

func (registry *KeyRegistry) attributeAuditActor(event *managementAuditEvent) {
	if key, exists := registry.state.Keys[event.key]; exists {
		if key.RotationTarget != "" {
			key = registry.state.Keys[key.RotationTarget]
		}
		if key.UserID != "" {
			event.actor = key.UserID
		}
	}
}

