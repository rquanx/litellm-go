package proxyconfig

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type policyRule struct {
	Inherit   string                     `yaml:"inherit"`
	Pipeline  *runtime.GuardrailPipeline `yaml:"pipeline"`
	Condition struct {
		Model any `yaml:"model"`
	} `yaml:"condition"`
	Guardrails struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"guardrails"`
}

type policyAttachment struct {
	Policy string   `yaml:"policy" json:"policy_name"`
	Scope  string   `yaml:"scope" json:"scope"`
	Teams  []string `yaml:"teams" json:"teams"`
	Keys   []string `yaml:"keys" json:"keys"`
	Models []string `yaml:"models" json:"models"`
	Tags   []string `yaml:"tags" json:"tags"`
}

type PolicyEngine struct {
	definitions map[string]policyRule
	policies    map[string]compiledPolicy
	attachments []policyAttachment
}

type compiledPolicy struct {
	chain    []string
	add      []string
	remove   []string
	models   []policyModelPattern
	pipeline *runtime.GuardrailPipeline
}

type policyModelPattern struct {
	exact string
	regex *regexp.Regexp
}

func compilePolicies(definitions map[string]policyRule, attachments []policyAttachment) (*PolicyEngine, error) {
	if len(definitions) == 0 && len(attachments) == 0 {
		return nil, nil
	}
	engine := &PolicyEngine{definitions: definitions, policies: make(map[string]compiledPolicy), attachments: attachments}
	active := make(map[string]bool)
	var compile func(string) (compiledPolicy, error)
	compile = func(name string) (compiledPolicy, error) {
		if previous, ok := engine.policies[name]; ok {
			return previous, nil
		}
		rule, ok := definitions[name]
		if !ok || name == "" || active[name] {
			return compiledPolicy{}, errors.New("undefined or cyclic policy inheritance")
		}
		active[name] = true
		defer delete(active, name)
		result := compiledPolicy{}
		if rule.Pipeline != nil {
			if err := rule.Pipeline.Validate(); err != nil {
				return result, err
			}
			result.pipeline = rule.Pipeline
		}
		if rule.Inherit != "" {
			parent, err := compile(rule.Inherit)
			if err != nil {
				return result, err
			}
			result.chain = slices.Clone(parent.chain)
		}
		result.chain = append(result.chain, name)
		for _, guard := range rule.Guardrails.Add {
			if guard == "" {
				return result, errors.New("policy guardrail name is empty")
			}
		}
		result.add = rule.Guardrails.Add
		result.remove = rule.Guardrails.Remove
		addPattern := func(value string) {
			pattern, _ := regexp.Compile("^(?:" + value + ")$")
			result.models = append(result.models, policyModelPattern{exact: value, regex: pattern})
		}
		switch value := rule.Condition.Model.(type) {
		case nil:
		case string:
			addPattern(value)
		case []any:
			result.models = []policyModelPattern{}
			for _, item := range value {
				model, ok := item.(string)
				if !ok {
					return result, errors.New("invalid policy model list")
				}
				addPattern(model)
			}
		default:
			return result, errors.New("invalid policy model condition")
		}
		engine.policies[name] = result
		return result, nil
	}
	for name := range definitions {
		if _, err := compile(name); err != nil {
			return nil, err
		}
	}
	for _, a := range attachments {
		if _, ok := engine.policies[a.Policy]; !ok || (a.Scope != "" && a.Scope != "*") {
			return nil, errors.New("invalid policy attachment")
		}
	}
	return engine, nil
}

type policyContext struct {
	Team  string   `json:"team_alias"`
	Key   string   `json:"key_alias"`
	Model string   `json:"model"`
	Tags  []string `json:"tags"`
}

func policyMatches(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	return slices.ContainsFunc(patterns, func(pattern string) bool {
		if value == "" {
			return pattern == "*"
		}
		prefix, wildcard := strings.CutSuffix(pattern, "*")
		if wildcard {
			return strings.HasPrefix(value, prefix)
		}
		return value == pattern
	})
}

func (a policyAttachment) match(input policyContext) (string, bool) {
	if a.Scope == "*" {
		return "scope:*", true
	}
	if !policyMatches(a.Teams, input.Team) || !policyMatches(a.Keys, input.Key) || !policyMatches(a.Models, input.Model) {
		return "", false
	}
	var reasons []string
	if len(a.Tags) > 0 {
		index := slices.IndexFunc(input.Tags, func(tag string) bool { return policyMatches(a.Tags, tag) })
		if index < 0 {
			return "", false
		}
		reasons = append(reasons, "tag:"+input.Tags[index])
	}
	if len(a.Teams) > 0 && input.Team != "" {
		reasons = append(reasons, "team:"+input.Team)
	}
	if len(a.Keys) > 0 && input.Key != "" {
		reasons = append(reasons, "key:"+input.Key)
	}
	if len(a.Models) > 0 && input.Model != "" {
		reasons = append(reasons, "model:"+input.Model)
	}
	if len(reasons) == 0 {
		return "scope:default", true
	}
	return strings.Join(reasons, "+"), true
}

func (rule compiledPolicy) matchesModel(model string) bool {
	return rule.models == nil || (model != "" && slices.ContainsFunc(rule.models, func(pattern policyModelPattern) bool {
		return pattern.exact == model || (pattern.regex != nil && pattern.regex.MatchString(model))
	}))
}

type policyMatchDetail struct {
	Name       string   `json:"policy_name"`
	Reason     string   `json:"matched_via"`
	Guardrails []string `json:"guardrails_added"`
}

type policyResolution struct {
	Guardrails []string            `json:"effective_guardrails"`
	Matches    []policyMatchDetail `json:"matched_policies"`
}

func (engine *PolicyEngine) resolve(input policyContext, preview bool) policyResolution {
	result := policyResolution{Guardrails: []string{}, Matches: []policyMatchDetail{}}
	if engine == nil {
		return result
	}
	seen := make(map[string]bool)
	effective := make(map[string]bool)
	for _, a := range engine.attachments {
		reason, matches := a.match(input)
		rule := engine.policies[a.Policy]
		if !matches || seen[a.Policy] || (preview && !rule.matchesModel(input.Model)) {
			continue
		}
		seen[a.Policy] = true
		detail := policyMatchDetail{Name: a.Policy, Reason: reason, Guardrails: engine.resolveRule(rule, &input)}
		for _, guard := range detail.Guardrails {
			effective[guard] = true
		}
		result.Matches = append(result.Matches, detail)
	}
	for guard := range effective {
		result.Guardrails = append(result.Guardrails, guard)
	}
	slices.Sort(result.Guardrails)
	return result
}

func (engine *PolicyEngine) resolveRule(rule compiledPolicy, input *policyContext) []string {
	guards := make(map[string]bool)
	for _, name := range rule.chain {
		part := engine.policies[name]
		if input != nil && !part.matchesModel(input.Model) {
			continue
		}
		for _, guard := range part.add {
			guards[guard] = true
		}
		for _, guard := range part.remove {
			delete(guards, guard)
		}
	}
	result := make([]string, 0, len(guards))
	for guard := range guards {
		result = append(result, guard)
	}
	slices.Sort(result)
	return result
}

func (keys *KeyRegistry) policyContext(r *http.Request, model string) policyContext {
	input := policyContext{Model: model}
	token := runtime.ProxyCredential(r)
	keys.mu.RLock()
	defer keys.mu.RUnlock()
	if _, _, ok := keys.authorizeLocked(token); !ok {
		return input
	}
	key := keys.state.Keys[keyDigest(token)]
	if key.RotationTarget != "" {
		key = keys.state.Keys[key.RotationTarget]
	}
	teamID := key.TeamID
	if teamID == "" && key.ProjectID != "" {
		teamID = keys.state.Projects[key.ProjectID].TeamID
	}
	team := keys.state.Teams[teamID]
	input.Team, input.Key = team.Alias, key.Alias
	input.Tags = append(policyMetadataTags(key.Metadata), policyMetadataTags(team.Metadata)...)
	return input
}

func policyMetadataTags(metadata map[string]any) []string {
	switch tags := metadata["tags"].(type) {
	case []string:
		return slices.Clone(tags)
	case []any:
		result := make([]string, 0, len(tags))
		for _, value := range tags {
			if tag, ok := value.(string); ok {
				result = append(result, tag)
			}
		}
		return result
	default:
		return nil
	}
}
