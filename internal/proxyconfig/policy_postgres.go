package proxyconfig

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/rquanx/litellm-go/internal/runtime"
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

func (registry *PolicyRegistry) Resolver(keys *KeyRegistry, master string) func(*http.Request, string, map[string]any) runtime.GuardrailPolicy {
	if registry.static == nil && registry.db == nil && keys == nil {
		return nil
	}
	return func(r *http.Request, model string, payload map[string]any) runtime.GuardrailPolicy {
		engine := registry.current.Load()
		input := policyContext{Model: model}
		if keys != nil {
			input = keys.policyContext(r, model)
		}
		if engine == nil {
			return runtime.GuardrailPolicy{Guardrails: input.guardrails}
		}
		for _, field := range []string{"metadata", "litellm_metadata"} {
			metadata, _ := payload[field].(map[string]any)
			input.Tags = append(input.Tags, policyMetadataTags(metadata)...)
		}
		input.Tags = append(input.Tags, policyStringList(payload["tags"])...)
		admin := master != "" && subtle.ConstantTimeCompare([]byte(master), []byte(runtime.ProxyCredential(r))) == 1
		var overrides map[string]policyRule
		for _, name := range policyStringList(payload["policies"]) {
			if id, versioned := strings.CutPrefix(name, "policy_"); versioned {
				if !admin {
					return runtime.GuardrailPolicy{Error: "Policy version selection requires the proxy master key", Status: http.StatusForbidden}
				}
				version, ok := engine.versions[strings.TrimSpace(id)]
				if !ok {
					continue
				}
				if overrides == nil {
					overrides = maps.Clone(engine.definitions)
				}
				overrides[version.name] = version.rule
				name = version.name
			}
			input.extra = append(input.extra, policyAttachment{Policy: name, source: "request"})
		}
		if overrides != nil {
			replaced, err := compilePolicies(overrides, engine.attachments)
			if err != nil {
				return runtime.GuardrailPolicy{Error: "Could not resolve the selected policy version", Status: http.StatusBadRequest}
			}
			engine = replaced
		}
		resolved := engine.resolve(input, true)
		result := runtime.GuardrailPolicy{Guardrails: resolved.Guardrails, Matches: make([]runtime.GuardrailPolicyMatch, 0, len(resolved.Matches))}
		for _, match := range resolved.Matches {
			result.Matches = append(result.Matches, runtime.GuardrailPolicyMatch{Name: match.Name, Reason: match.Reason})
			if pipeline := engine.policies[match.Name].pipeline; pipeline != nil && (match.Reason != "request" || admin) {
				id := engine.policies[match.Name].versionID
				if id == "" {
					id = match.Name
				}
				result.Pipelines = append(result.Pipelines, runtime.PolicyPipeline{Name: match.Name, ID: id, Spec: *pipeline})
			}
		}
		return result
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

func (registry *PolicyRegistry) RunRefresh(ctx context.Context) {
	if registry.db == nil {
		return
	}
	refreshPostgresRegistry(ctx, "policy", registry.refresh)
}

func refreshPostgresRegistry(ctx context.Context, name string, refresh func(context.Context) error) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := refresh(refreshCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("could not refresh PostgreSQL %s snapshot", name)
			}
		}
	}
}

func (registry *PolicyRegistry) compileSnapshot(ctx context.Context, tx *sql.Tx) (*PolicyEngine, error) {
	definitions := make(map[string]policyRule)
	var attachments []policyAttachment
	if registry.static != nil {
		maps.Copy(definitions, registry.static.definitions)
		attachments = slices.Clone(registry.static.attachments)
	}
	versions, err := readPolicyDefinitions(ctx, tx, definitions)
	if err != nil {
		return nil, err
	}
	dynamic, err := readPolicyAttachments(ctx, tx)
	if err != nil {
		return nil, err
	}
	engine, err := compilePolicies(definitions, append(attachments, dynamic...))
	if err != nil {
		return nil, err
	}
	if engine == nil && len(versions) > 0 {
		engine = &PolicyEngine{definitions: definitions}
	}
	if engine != nil {
		engine.versions = versions
	}
	return engine, nil
}

func readPolicyDefinitions(ctx context.Context, tx *sql.Tx, definitions map[string]policyRule) (map[string]policyVersion, error) {
	rows, err := tx.QueryContext(ctx, `SELECT policy_id,policy_name,version_status,COALESCE(inherit,''),guardrails_add,guardrails_remove,condition,pipeline
		FROM `+policyTable+` WHERE version_status IN ('production','draft','published') ORDER BY policy_name,version_number DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	production := make(map[string]bool)
	versions := make(map[string]policyVersion)
	for rows.Next() {
		var id, name, status string
		var rule policyRule
		var condition, pipeline []byte
		if err := rows.Scan(&id, &name, &status, &rule.Inherit, pq.Array(&rule.Guardrails.Add), pq.Array(&rule.Guardrails.Remove), &condition, &pipeline); err != nil {
			return nil, err
		}
		rule.versionID = id
		if len(pipeline) > 0 && decodePolicyCondition(pipeline, &rule.Pipeline) != nil {
			return nil, errors.New("invalid database guardrail pipeline: " + name)
		}
		if len(condition) > 0 && decodePolicyCondition(condition, &rule.Condition) != nil {
			return nil, errors.New("invalid database policy condition: " + name)
		}
		if status != "production" {
			versions[id] = policyVersion{name: name, rule: rule}
		} else if !production[name] {
			definitions[name] = rule
			production[name] = true
		}
	}
	return versions, rows.Err()
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
