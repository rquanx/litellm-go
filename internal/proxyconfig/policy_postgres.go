package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

const policyTable = `"LiteLLM_PolicyTable"`
const policySchema = ` (
	policy_id TEXT PRIMARY KEY, policy_name TEXT NOT NULL,
	version_number INTEGER NOT NULL DEFAULT 1, version_status TEXT NOT NULL DEFAULT 'production',
	parent_version_id TEXT, is_latest BOOLEAN NOT NULL DEFAULT true,
	published_at TIMESTAMP(3), production_at TIMESTAMP(3),
	inherit TEXT, description TEXT,
	guardrails_add TEXT[] NOT NULL DEFAULT '{}', guardrails_remove TEXT[] NOT NULL DEFAULT '{}',
	condition JSONB DEFAULT '{}', pipeline JSONB,
	created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), created_by TEXT,
	updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), updated_by TEXT,
	UNIQUE (policy_name,version_number)
)`
const policyAttachmentTable = `"LiteLLM_PolicyAttachmentTable"`
const policyAttachmentSchema = ` (
	attachment_id TEXT PRIMARY KEY, policy_name TEXT NOT NULL, scope TEXT,
	teams TEXT[] NOT NULL DEFAULT '{}', keys TEXT[] NOT NULL DEFAULT '{}',
	models TEXT[] NOT NULL DEFAULT '{}', tags TEXT[] NOT NULL DEFAULT '{}',
	created_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), created_by TEXT,
	updated_at TIMESTAMP(3) NOT NULL DEFAULT (CURRENT_TIMESTAMP AT TIME ZONE 'UTC'), updated_by TEXT
)`

type PolicyRegistry struct {
	db      *sql.DB
	static  *PolicyEngine
	mu      sync.Mutex
	current atomic.Pointer[PolicyEngine]
}

func OpenPolicyRegistry(static *PolicyEngine, db *sql.DB) (*PolicyRegistry, error) {
	registry := &PolicyRegistry{static: static, db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if db != nil {
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+policyTable+policySchema); err != nil {
			return nil, err
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+policyAttachmentTable+policyAttachmentSchema); err != nil {
			return nil, err
		}
	}
	if err := registry.refresh(ctx); err != nil {
		return nil, err
	}
	return registry, nil
}

func (registry *PolicyRegistry) GuardrailsFor(keys *KeyRegistry) func(*http.Request, string) []string {
	if registry.static == nil && registry.db == nil {
		return nil
	}
	return func(r *http.Request, model string) []string {
		engine := registry.current.Load()
		if engine == nil {
			return nil
		}
		return engine.guardrailsFor(keys, r, model)
	}
}

func (registry *PolicyRegistry) refresh(ctx context.Context) error {
	if registry.db == nil {
		registry.current.Store(registry.static)
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	tx, err := registry.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	engine, err := registry.compileSnapshot(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	registry.current.Store(engine)
	return nil
}

func (registry *PolicyRegistry) compileSnapshot(ctx context.Context, tx *sql.Tx) (*PolicyEngine, error) {
	definitions := make(map[string]policyRule)
	var attachments []policyAttachment
	if registry.static != nil {
		maps.Copy(definitions, registry.static.definitions)
		attachments = slices.Clone(registry.static.attachments)
	}
	var hasPolicies, hasAttachments bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL,to_regclass($2) IS NOT NULL`, policyTable, policyAttachmentTable).
		Scan(&hasPolicies, &hasAttachments); err != nil {
		return nil, err
	}
	if hasPolicies {
		if err := readProductionPolicies(ctx, tx, definitions); err != nil {
			return nil, err
		}
	}
	if hasAttachments {
		dynamic, err := readPolicyAttachments(ctx, tx)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, dynamic...)
	}
	return compilePolicies(definitions, attachments)
}

func readProductionPolicies(ctx context.Context, tx *sql.Tx, definitions map[string]policyRule) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (policy_name) policy_name,COALESCE(inherit,''),guardrails_add,guardrails_remove,condition,pipeline
		FROM `+policyTable+` WHERE version_status='production' ORDER BY policy_name,version_number DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var rule policyRule
		var condition, pipeline []byte
		if err := rows.Scan(&name, &rule.Inherit, pq.Array(&rule.Guardrails.Add), pq.Array(&rule.Guardrails.Remove), &condition, &pipeline); err != nil {
			return err
		}
		if len(pipeline) > 0 && string(pipeline) != "null" {
			return errors.New("production policy uses an unsupported guardrail pipeline: " + name)
		}
		if len(condition) > 0 && decodePolicyCondition(condition, &rule.Condition) != nil {
			return errors.New("invalid database policy condition: " + name)
		}
		definitions[name] = rule
	}
	return rows.Err()
}

func decodePolicyCondition(data []byte, target any) error {
	if len(data) > 0 && data[0] == '"' {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return err
		}
		data = []byte(encoded)
	}
	return json.Unmarshal(data, target)
}

func readPolicyAttachments(ctx context.Context, tx *sql.Tx) ([]policyAttachment, error) {
	rows, err := tx.QueryContext(ctx, `SELECT policy_name,COALESCE(scope,''),teams,keys,models,tags
		FROM `+policyAttachmentTable+` ORDER BY created_at DESC,attachment_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var attachments []policyAttachment
	for rows.Next() {
		var a policyAttachment
		if err := rows.Scan(&a.Policy, &a.Scope, pq.Array(&a.Teams), pq.Array(&a.Keys), pq.Array(&a.Models), pq.Array(&a.Tags)); err != nil {
			return nil, err
		}
		attachments = append(attachments, a)
	}
	return attachments, rows.Err()
}
