package proxyconfig

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/lib/pq"
)

const attachmentColumns = "attachment_id,policy_name,scope,teams,keys,models,tags,created_at,updated_at,created_by,updated_by"

type policyAttachmentRecord struct {
	ID        string    `json:"attachment_id"`
	Policy    string    `json:"policy_name"`
	Scope     *string   `json:"scope"`
	Teams     []string  `json:"teams"`
	Keys      []string  `json:"keys"`
	Models    []string  `json:"models"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy *string   `json:"created_by"`
	UpdatedBy *string   `json:"updated_by"`
}

func scanPolicyAttachment(row interface{ Scan(...any) error }) (policyAttachmentRecord, error) {
	var a policyAttachmentRecord
	err := row.Scan(&a.ID, &a.Policy, &a.Scope, pq.Array(&a.Teams), pq.Array(&a.Keys), pq.Array(&a.Models), pq.Array(&a.Tags),
		&a.CreatedAt, &a.UpdatedAt, &a.CreatedBy, &a.UpdatedBy)
	return a, err
}

func (registry *PolicyRegistry) attachmentEndpoint(w http.ResponseWriter, r *http.Request) {
	if registry.db == nil {
		modelError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := r.PathValue("attachment_id")
	if r.Method == http.MethodGet {
		registry.readAttachments(ctx, w, id)
		return
	}
	var spec policyAttachment
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := decodeModelJSON(r.Body, &spec); err != nil || spec.Policy == "" || (spec.Scope != "" && spec.Scope != "*") {
			modelError(w, http.StatusUnprocessableEntity, "Expected a policy name and a valid attachment scope")
			return
		}
	}
	result, err := registry.changeAttachment(ctx, id, spec)
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusNotFound, "Policy or attachment not found")
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not change policy attachment")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func (registry *PolicyRegistry) readAttachments(ctx context.Context, w http.ResponseWriter, id string) {
	const query = "SELECT " + attachmentColumns + " FROM " + policyAttachmentTable
	if id != "" {
		record, err := scanPolicyAttachment(registry.db.QueryRowContext(ctx, query+" WHERE attachment_id=$1", id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			modelError(w, http.StatusNotFound, "Policy attachment not found")
		case err != nil:
			modelError(w, http.StatusInternalServerError, "Could not read policy attachment")
		default:
			modelJSON(w, http.StatusOK, record)
		}
		return
	}
	rows, err := registry.db.QueryContext(ctx, query+" ORDER BY created_at DESC,attachment_id")
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not list policy attachments")
		return
	}
	defer rows.Close()
	result := []policyAttachmentRecord{}
	for rows.Next() {
		record, err := scanPolicyAttachment(rows)
		if err != nil {
			modelError(w, http.StatusInternalServerError, "Could not read policy attachment")
			return
		}
		result = append(result, record)
	}
	if rows.Err() != nil {
		modelError(w, http.StatusInternalServerError, "Could not list policy attachments")
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"attachments": result, "total_count": len(result)})
}

func (registry *PolicyRegistry) changeAttachment(ctx context.Context, id string, spec policyAttachment) (any, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	tx, err := registry.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var result any
	if id == "" {
		current, err := registry.compileSnapshot(ctx, tx)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, sql.ErrNoRows
		}
		if _, ok := current.policies[spec.Policy]; !ok {
			return nil, sql.ErrNoRows
		}
		record, err := scanPolicyAttachment(tx.QueryRowContext(ctx, `INSERT INTO `+policyAttachmentTable+`
			(attachment_id,policy_name,scope,teams,keys,models,tags)
			VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7) RETURNING `+attachmentColumns,
			newEntityID(), spec.Policy, spec.Scope, pq.Array(append([]string{}, spec.Teams...)),
			pq.Array(append([]string{}, spec.Keys...)), pq.Array(append([]string{}, spec.Models...)), pq.Array(append([]string{}, spec.Tags...))))
		if err != nil {
			return nil, err
		}
		result = record
	} else {
		var deleted string
		if err := tx.QueryRowContext(ctx, `DELETE FROM `+policyAttachmentTable+` WHERE attachment_id=$1 RETURNING attachment_id`, id).Scan(&deleted); err != nil {
			return nil, err
		}
		result = map[string]string{"message": "Attachment " + deleted + " deleted successfully"}
	}
	engine, err := registry.compileSnapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	registry.current.Store(engine)
	return result, nil
}
