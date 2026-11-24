package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
)

var errPolicyDraftRequired = errors.New("only draft policies can be updated")
var errPolicyVersionTransition = errors.New("invalid policy version transition")

type policyMutation struct {
	Name        *string  `json:"policy_name"`
	Inherit     *string  `json:"inherit"`
	Description *string  `json:"description"`
	Add         []string `json:"guardrails_add"`
	Remove      []string `json:"guardrails_remove"`
	Condition   *struct {
		Model *string `json:"model"`
	} `json:"condition"`
	Pipeline *runtime.GuardrailPipeline `json:"pipeline"`
}

func (registry *PolicyRegistry) writeManagedPolicy(w http.ResponseWriter, r *http.Request) {
	if registry.db == nil {
		modelError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	var input *policyMutation
	var version struct {
		Source string `json:"source_policy_id"`
		Status string `json:"version_status"`
	}
	name := r.PathValue("policy_name")
	id := r.PathValue("policy_id")
	versionEndpoint := strings.HasSuffix(r.Pattern, "/status") || strings.HasSuffix(r.Pattern, "/versions")
	if versionEndpoint {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := decodeModelJSON(r.Body, &version); err != nil {
			modelError(w, http.StatusUnprocessableEntity, "Expected a version definition")
			return
		}
	} else if r.Method != http.MethodDelete {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := decodeModelJSON(r.Body, &input); err != nil || input == nil ||
			(r.Method == http.MethodPost && input.Name == nil) ||
			(input.Name != nil && strings.TrimSpace(*input.Name) == "") {
			modelError(w, http.StatusUnprocessableEntity, "Expected a policy definition")
			return
		}
		if input.Pipeline != nil && input.Pipeline.Validate() != nil {
			modelError(w, http.StatusUnprocessableEntity, "Invalid guardrail pipeline")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	value, err := registry.mutatePolicy(ctx, func(tx *sql.Tx) (any, error) {
		switch {
		case versionEndpoint && name != "":
			return clonePolicyVersion(ctx, tx, name, version.Source)
		case versionEndpoint:
			return setPolicyVersionStatus(ctx, tx, id, version.Status)
		case r.Method == http.MethodDelete:
			return deleteManagedPolicy(ctx, tx, id, name)
		default:
			return storePolicyDefinition(ctx, tx, id, input)
		}
	})
	responseStatus := http.StatusOK
	var postgresErr *pq.Error
	switch {
	case errors.Is(err, sql.ErrNoRows):
		responseStatus = http.StatusNotFound
	case errors.Is(err, errPolicyDraftRequired), errors.Is(err, errPolicyVersionTransition):
		responseStatus = http.StatusBadRequest
	case errors.As(err, &postgresErr) && postgresErr.Code == "23505":
		responseStatus = http.StatusBadRequest
	case err != nil:
		responseStatus = http.StatusInternalServerError
	}
	if err != nil {
		modelError(w, responseStatus, "Could not change policy")
		return
	}
	modelJSON(w, responseStatus, value)
}

func (registry *PolicyRegistry) mutatePolicy(ctx context.Context, change func(*sql.Tx) (any, error)) (any, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	tx, err := registry.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := change(tx)
	if err != nil {
		return nil, err
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

func deleteManagedPolicy(ctx context.Context, tx *sql.Tx, id, name string) (any, error) {
	if name != "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM `+policyTable+` WHERE policy_name=$1`, name)
		return map[string]string{"message": "All versions of policy '" + name + "' deleted successfully"}, err
	}
	var previous string
	if err := tx.QueryRowContext(ctx, `DELETE FROM `+policyTable+` WHERE policy_id=$1 RETURNING version_status`, id).Scan(&previous); err != nil {
		return nil, err
	}
	response := map[string]string{"message": "Policy " + id + " deleted successfully"}
	if previous == "production" {
		response["warning"] = "Production version was deleted. Promote another version to production if this policy should remain active."
	}
	return response, nil
}

func clonePolicyVersion(ctx context.Context, tx *sql.Tx, name, source string) (json.RawMessage, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE `+policyTable+` SET is_latest=false WHERE policy_name=$1 AND is_latest`, name); err != nil {
		return nil, err
	}
	var record json.RawMessage
	err := tx.QueryRowContext(ctx, `INSERT INTO `+policyTable+` AS p
		(policy_id,policy_name,version_number,version_status,parent_version_id,inherit,description,guardrails_add,guardrails_remove,condition,pipeline)
		SELECT $3,policy_name,(SELECT MAX(version_number)+1 FROM `+policyTable+` WHERE policy_name=$1),'draft',policy_id,
			inherit,description,guardrails_add,guardrails_remove,condition,pipeline
		FROM `+policyTable+`
		WHERE policy_name=$1 AND (($2='' AND version_status='production') OR policy_id=NULLIF($2,''))
		ORDER BY version_number DESC LIMIT 1 RETURNING to_jsonb(p)`, name, source, newEntityID()).Scan(&record)
	return normalizePolicyRecord(record, err)
}

func setPolicyVersionStatus(ctx context.Context, tx *sql.Tx, id, status string) (json.RawMessage, error) {
	var name, current string
	if err := tx.QueryRowContext(ctx, `SELECT policy_name,version_status FROM `+policyTable+` WHERE policy_id=$1 FOR UPDATE`, id).Scan(&name, &current); err != nil {
		return nil, err
	}
	if !((status == "published" && current == "draft") || (status == "production" && current == "published")) {
		return nil, errPolicyVersionTransition
	}
	if status == "production" {
		if _, err := tx.ExecContext(ctx, `UPDATE `+policyTable+` SET version_status='published',updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
			WHERE policy_name=$1 AND version_status='production'`, name); err != nil {
			return nil, err
		}
	}
	var record json.RawMessage
	err := tx.QueryRowContext(ctx, `UPDATE `+policyTable+` p SET version_status=$2,
		published_at=CASE WHEN $2='published' THEN CURRENT_TIMESTAMP AT TIME ZONE 'UTC' ELSE published_at END,
		production_at=CASE WHEN $2='production' THEN CURRENT_TIMESTAMP AT TIME ZONE 'UTC' ELSE production_at END,
		updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
		WHERE policy_id=$1 RETURNING to_jsonb(p)`, id, status).Scan(&record)
	return normalizePolicyRecord(record, err)
}

func storePolicyDefinition(ctx context.Context, tx *sql.Tx, id string, input *policyMutation) (json.RawMessage, error) {
	var condition any
	if input.Condition != nil {
		raw, err := json.Marshal(input.Condition)
		if err != nil {
			return nil, err
		}
		condition = string(raw)
	}
	var pipeline any
	if input.Pipeline != nil {
		raw, err := json.Marshal(input.Pipeline)
		if err != nil {
			return nil, err
		}
		pipeline = string(raw)
	}
	var record json.RawMessage
	if id == "" {
		err := tx.QueryRowContext(ctx, `INSERT INTO `+policyTable+` AS p
			(policy_id,policy_name,inherit,description,guardrails_add,guardrails_remove,condition,pipeline,production_at)
			VALUES ($1,$2,NULLIF($3,''),$4,COALESCE($5::text[],'{}'),COALESCE($6::text[],'{}'),$7::jsonb,$8::jsonb,CURRENT_TIMESTAMP AT TIME ZONE 'UTC')
			RETURNING to_jsonb(p)`, newEntityID(), input.Name, input.Inherit, input.Description, pq.Array(input.Add), pq.Array(input.Remove), condition, pipeline).Scan(&record)
		return normalizePolicyRecord(record, err)
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT version_status FROM `+policyTable+` WHERE policy_id=$1 FOR UPDATE`, id).Scan(&status); err != nil {
		return nil, err
	}
	if status != "draft" {
		return nil, errPolicyDraftRequired
	}
	err := tx.QueryRowContext(ctx, `UPDATE `+policyTable+` p SET
		policy_name=COALESCE($2,policy_name),inherit=CASE WHEN $3::text IS NULL THEN inherit ELSE NULLIF($3,'') END,
		description=COALESCE($4,description),guardrails_add=COALESCE($5,guardrails_add),guardrails_remove=COALESCE($6,guardrails_remove),
		condition=COALESCE($7,condition),pipeline=COALESCE($8,pipeline),updated_at=CURRENT_TIMESTAMP AT TIME ZONE 'UTC'
		WHERE policy_id=$1 RETURNING to_jsonb(p)`, id, input.Name, input.Inherit, input.Description, pq.Array(input.Add), pq.Array(input.Remove), condition, pipeline).Scan(&record)
	return normalizePolicyRecord(record, err)
}
