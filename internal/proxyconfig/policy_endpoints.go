package proxyconfig

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func (registry *PolicyRegistry) Handler(next http.Handler, master string, keys *KeyRegistry) http.Handler {
	mux := http.NewServeMux()
	protected := func(target *http.ServeMux, pattern string, handler func(*PolicyEngine, http.ResponseWriter, *http.Request)) {
		target.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if master == "" || subtle.ConstantTimeCompare([]byte(master), []byte(runtime.ProxyCredential(r))) != 1 {
				modelError(w, http.StatusUnauthorized, "Proxy master key required")
				return
			}
			if r.URL.Path == "/policies/resolve" && r.URL.Query().Get("force_sync") == "true" {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				if err := registry.refresh(ctx); err != nil {
					modelError(w, http.StatusInternalServerError, "Could not load database policies")
					return
				}
			}
			handler(registry.current.Load(), w, r)
		})
	}
	protected(mux, "POST /policies/resolve", (*PolicyEngine).previewPolicy)
	protected(mux, "POST /policy/test", (*PolicyEngine).previewPolicy)
	protected(mux, "GET /policy/list", (*PolicyEngine).listPolicies)
	protected(mux, "GET /policy/info/{policy_name}", (*PolicyEngine).listPolicies)
	for _, pattern := range []string{
		"GET /policies/list", "GET /policies/{policy_id}", "GET /policies/name/{policy_name}/versions",
		"GET /policies/{policy_id}/resolved-guardrails", "GET /policies/compare",
	} {
		protected(mux, pattern, func(engine *PolicyEngine, w http.ResponseWriter, r *http.Request) {
			registry.readManagedPolicy(engine, w, r)
		})
	}
	for _, pattern := range []string{
		"POST /policies", "PUT /policies/{policy_id}", "DELETE /policies/{policy_id}",
		"POST /policies/name/{policy_name}/versions", "PUT /policies/{policy_id}/status", "DELETE /policies/name/{policy_name}/all-versions",
	} {
		protected(mux, pattern, func(_ *PolicyEngine, w http.ResponseWriter, r *http.Request) {
			registry.writeManagedPolicy(w, r)
		})
	}
	attachments := http.NewServeMux()
	protected(attachments, "POST /policies/attachments/estimate-impact", func(_ *PolicyEngine, w http.ResponseWriter, r *http.Request) {
		estimatePolicyImpact(w, r, keys)
	})
	for _, pattern := range []string{
		"GET /policies/attachments/list", "GET /policies/attachments/{attachment_id}",
		"POST /policies/attachments", "DELETE /policies/attachments/{attachment_id}",
	} {
		protected(attachments, pattern, func(_ *PolicyEngine, w http.ResponseWriter, r *http.Request) {
			registry.attachmentEndpoint(w, r)
		})
	}
	mux.Handle("/", next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/policies/attachments" || strings.HasPrefix(r.URL.Path, "/policies/attachments/") {
			attachments.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (engine *PolicyEngine) previewPolicy(w http.ResponseWriter, r *http.Request) {
	var input *policyContext
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := decodeModelJSON(r.Body, &input); err != nil || input == nil {
		modelError(w, http.StatusUnprocessableEntity, "Expected a policy context")
		return
	}
	if r.URL.Path == "/policies/resolve" {
		modelJSON(w, http.StatusOK, engine.resolve(*input, true))
		return
	}
	resolved := engine.resolve(*input, false)
	policies := make([]string, 0, len(resolved.Matches))
	for _, match := range resolved.Matches {
		policies = append(policies, match.Name)
	}
	var message any
	if engine == nil || len(engine.policies) == 0 {
		message = "Policy engine not initialized. No policies loaded."
	}
	modelJSON(w, http.StatusOK, map[string]any{"context": input, "matching_policies": policies, "resolved_guardrails": resolved.Guardrails, "message": message})
}

func (engine *PolicyEngine) listPolicies(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("policy_name")
	if name != "" {
		if engine != nil {
			if rule, ok := engine.policies[name]; ok {
				entry := engine.describePolicy(rule)
				entry["policy_name"] = name
				modelJSON(w, http.StatusOK, entry)
				return
			}
		}
		modelError(w, http.StatusNotFound, "Policy not found")
		return
	}
	result := make(map[string]map[string]any)
	if engine != nil {
		for name, rule := range engine.policies {
			result[name] = engine.describePolicy(rule)
		}
	}
	modelJSON(w, http.StatusOK, map[string]any{"policies": result, "total_count": len(result)})
}

func (engine *PolicyEngine) describePolicy(rule compiledPolicy) map[string]any {
	var parent any
	if len(rule.chain) > 1 {
		parent = rule.chain[len(rule.chain)-2]
	}
	return map[string]any{
		"inherit":             parent,
		"scope":               map[string][]string{"teams": {}, "keys": {}, "models": {}, "tags": {}},
		"guardrails":          map[string][]string{"add": append([]string{}, rule.add...), "remove": append([]string{}, rule.remove...)},
		"resolved_guardrails": engine.resolveRule(rule, nil),
		"inheritance_chain":   rule.chain,
	}
}
