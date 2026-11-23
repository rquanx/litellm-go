package proxyconfig

import (
	"crypto/subtle"
	"net/http"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func (engine *PolicyEngine) Handler(next http.Handler, master string) http.Handler {
	mux := http.NewServeMux()
	protected := func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if master == "" || subtle.ConstantTimeCompare([]byte(master), []byte(runtime.ProxyCredential(r))) != 1 {
				modelError(w, http.StatusUnauthorized, "Proxy master key required")
				return
			}
			handler(w, r)
		})
	}
	protected("POST /policies/resolve", engine.previewPolicy)
	protected("POST /policy/test", engine.previewPolicy)
	protected("GET /policy/list", engine.listPolicies)
	protected("GET /policy/info/{policy_name}", engine.listPolicies)
	mux.Handle("/", next)
	return mux
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
