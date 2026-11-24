package proxyconfig

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type policyValidationIssue struct {
	Policy string `json:"policy_name"`
	Type string `json:"error_type"`
	Message string `json:"message"`
	Field *string `json:"field"`
	Value *string `json:"value"`
}

type policyValidationDefinition struct {
	Inherit string `json:"inherit"`
	Guardrails json.RawMessage `json:"guardrails"`
	Pipeline *runtime.GuardrailPipeline `json:"pipeline"`
	Condition *struct {
		Model any `json:"model"`
	} `json:"condition"`
	Add, Remove []string
}

func validatePolicyConfiguration(w http.ResponseWriter, r *http.Request, guardrails []string) {
	var input struct {
		Policies map[string]json.RawMessage `json:"policies"`
	}
	if decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input) != nil || input.Policies == nil {
		modelError(w, http.StatusUnprocessableEntity, "Expected a policies object")
		return
	}
	issues, warnings := []policyValidationIssue{}, []policyValidationIssue{}
	write := func() {
		modelJSON(w, http.StatusOK, map[string]any{"valid": len(issues) == 0, "errors": issues, "warnings": warnings})
	}
	definitions := make(map[string]policyValidationDefinition, len(input.Policies))
	names := slices.Sorted(maps.Keys(input.Policies))
	for _, name := range names {
		definition, err := parsePolicyValidationDefinition(input.Policies[name])
		if err != nil {
			issues = append(issues, policyValidationIssue{Policy: name, Type: "invalid_syntax", Message: "Failed to parse policy: "+err.Error()})
		} else {
			definitions[name] = definition
		}
	}
	if len(issues) != 0 {
		write()
		return
	}
	known := make(map[string]bool, len(guardrails))
	for _, name := range guardrails {
		known[name] = true
	}
	for _, name := range names {
		definition := definitions[name]
		addIssue := func(target *[]policyValidationIssue, message, field, value string) {
			*target = append(*target, policyValidationIssue{
				Policy: name, Type: "invalid_guardrail", Message: message, Field: &field, Value: &value,
			})
		}
		for _, guard := range definition.Add {
			if len(known) > 0 && !known[guard] {
				addIssue(&issues, fmt.Sprintf("Guardrail '%s' not found in guardrail registry", guard), "guardrails.add", guard)
			}
		}
		for _, guard := range definition.Remove {
			if len(known) > 0 && !known[guard] {
				addIssue(&warnings, fmt.Sprintf("Guardrail '%s' in remove list not found in guardrail registry", guard), "guardrails.remove", guard)
			}
		}
		if definition.Pipeline != nil {
			for i, step := range definition.Pipeline.Steps {
				if !slices.Contains(definition.Add, step.Guardrail) {
					addIssue(&issues, fmt.Sprintf("Pipeline step %d guardrail '%s' is not in the policy's guardrails.add list", i, step.Guardrail), "pipeline.steps", step.Guardrail)
				}
				if len(known) > 0 && !known[step.Guardrail] {
					addIssue(&issues, fmt.Sprintf("Pipeline step %d guardrail '%s' not found in guardrail registry", i, step.Guardrail), "pipeline.steps", step.Guardrail)
				}
			}
		}
		if issue := policyInheritanceIssue(name, definitions); issue != nil {
			issues = append(issues, *issue)
		}
	}
	write()
}

func parsePolicyValidationDefinition(payload json.RawMessage) (policyValidationDefinition, error) {
	var decoded *policyValidationDefinition
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return policyValidationDefinition{}, err
	}
	if decoded == nil {
		return policyValidationDefinition{}, fmt.Errorf("policy must be an object")
	}
	switch raw := decoded.Guardrails; {
	case len(raw) == 0 || string(raw) == "null":
	case raw[0] == '[':
		if err := json.Unmarshal(raw, &decoded.Add); err != nil {
			return *decoded, err
		}
	default:
		var guards struct {
			Add, Remove []string
		}
		if err := json.Unmarshal(raw, &guards); err != nil {
			return *decoded, err
		}
		decoded.Add, decoded.Remove = guards.Add, guards.Remove
	}
	if decoded.Condition != nil {
		switch value := decoded.Condition.Model.(type) {
		case nil, string:
		case []any:
			for _, item := range value {
				if _, ok := item.(string); !ok {
					return *decoded, fmt.Errorf("condition.model must contain strings")
				}
			}
		default:
			return *decoded, fmt.Errorf("condition.model must be a string or array")
		}
	}
	if decoded.Pipeline != nil {
		if decoded.Pipeline.Mode == "" {
			decoded.Pipeline.Mode = "pre_call"
		}
		if err := decoded.Pipeline.Validate(); err != nil {
			return *decoded, err
		}
	}
	return *decoded, nil
}

func policyInheritanceIssue(name string, definitions map[string]policyValidationDefinition) *policyValidationIssue {
	field := "inherit"
	current := name
	visited := make(map[string]bool)
	chain := make([]string, 0, 8)
	for current != "" {
		issue := policyValidationIssue{Policy: current, Type: "circular_inheritance", Field: &field}
		if len(chain) == 100 {
			issue.Message = "Inheritance chain too deep (exceeded max depth of 100)"
			return &issue
		}
		if visited[current] {
			issue.Message = "Circular inheritance detected: "+strings.Join(append(chain, current), " -> ")
			return &issue
		}
		visited[current] = true
		chain = append(chain, current)
		parent := definitions[current].Inherit
		if _, exists := definitions[parent]; parent != "" && !exists {
			issue.Type, issue.Value = "invalid_inheritance", &parent
			issue.Message = fmt.Sprintf("Parent policy '%s' not found", parent)
			return &issue
		}
		current = parent
	}
	return nil
}
