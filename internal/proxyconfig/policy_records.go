package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"
)

func (registry *PolicyRegistry) readManagedPolicy(engine *PolicyEngine, w http.ResponseWriter, r *http.Request) {
	if registry.db == nil {
		modelError(w, http.StatusInternalServerError, "Database not connected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id, name := r.PathValue("policy_id"), r.PathValue("policy_name")
	var result any
	var err error
	switch {
	case r.URL.Path == "/policies/compare":
		a, b := r.URL.Query().Get("version_a"), r.URL.Query().Get("version_b")
		if a == "" || b == "" {
			modelError(w, http.StatusUnprocessableEntity, "Both version IDs are required")
			return
		}
		result, err = registry.comparePolicies(ctx, a, b)
	case id != "":
		var raw json.RawMessage
		raw, err = registry.readPolicyRecord(ctx, id)
		result = raw
		if err == nil && strings.HasSuffix(r.URL.Path, "/resolved-guardrails") {
			var record struct {
				Name string `json:"policy_name"`
			}
			err = json.Unmarshal(raw, &record)
			if err == nil {
				if engine == nil {
					err = sql.ErrNoRows
				} else if rule, ok := engine.policies[record.Name]; ok {
					result = map[string]any{"policy_id": id, "policy_name": record.Name, "resolved_guardrails": engine.resolveRule(rule, nil)}
				} else {
					err = sql.ErrNoRows
				}
			}
		}
	default:
		result, err = registry.listManagedPolicies(ctx, name, r.URL.Query().Get("version_status"))
	}
	if errors.Is(err, sql.ErrNoRows) {
		modelError(w, http.StatusNotFound, "Policy not found")
		return
	}
	if err != nil {
		modelError(w, http.StatusInternalServerError, "Could not read policies")
		return
	}
	modelJSON(w, http.StatusOK, result)
}

func (registry *PolicyRegistry) readPolicyRecord(ctx context.Context, id string) (json.RawMessage, error) {
	var record json.RawMessage
	err := registry.db.QueryRowContext(ctx, `SELECT to_jsonb(p) FROM `+policyTable+` p WHERE policy_id=$1`, id).Scan(&record)
	return normalizePolicyRecord(record, err)
}

func (registry *PolicyRegistry) listManagedPolicies(ctx context.Context, name, status string) (any, error) {
	rows, err := registry.db.QueryContext(ctx, `SELECT to_jsonb(p) FROM `+policyTable+` p
		WHERE ($1='' OR policy_name=$1) AND ($2='' OR version_status=$2)
		ORDER BY CASE WHEN $1<>'' THEN version_number END DESC,created_at DESC,policy_id`, name, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []json.RawMessage{}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		raw, err = normalizePolicyRecord(raw, nil)
		if err != nil {
			return nil, err
		}
		records = append(records, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if name != "" {
		return map[string]any{"policy_name": name, "versions": records, "total_count": len(records)}, nil
	}
	return map[string]any{"policies": records, "total_count": len(records)}, nil
}

func (registry *PolicyRegistry) comparePolicies(ctx context.Context, a, b string) (any, error) {
	var rawA, rawB json.RawMessage
	err := registry.db.QueryRowContext(ctx, `SELECT to_jsonb(a),to_jsonb(b) FROM `+policyTable+` a
		CROSS JOIN `+policyTable+` b WHERE a.policy_id=$1 AND b.policy_id=$2`, a, b).Scan(&rawA, &rawB)
	if err != nil {
		return nil, err
	}
	rawA, err = normalizePolicyRecord(rawA, nil)
	if err != nil {
		return nil, err
	}
	rawB, err = normalizePolicyRecord(rawB, nil)
	if err != nil {
		return nil, err
	}
	var left, right map[string]any
	if err := json.Unmarshal(rawA, &left); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawB, &right); err != nil {
		return nil, err
	}
	diffs := make(map[string]map[string]any)
	for _, field := range []string{"inherit", "description", "guardrails_add", "guardrails_remove", "condition", "pipeline"} {
		if !reflect.DeepEqual(left[field], right[field]) {
			diffs[field] = map[string]any{"version_a": left[field], "version_b": right[field]}
		}
	}
	return map[string]any{"version_a": rawA, "version_b": rawB, "field_diffs": diffs}, nil
}

func normalizePolicyRecord(raw json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, field := range []string{"condition", "pipeline"} {
		value := fields[field]
		if len(value) == 0 || value[0] != '"' {
			continue
		}
		var parsed json.RawMessage
		if err := decodePolicyCondition(value, &parsed); err != nil {
			return nil, err
		}
		fields[field] = parsed
	}
	return json.Marshal(fields)
}
