package proxyconfig

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type genericGuardrailSpec struct {
	Name   string `yaml:"guardrail_name" json:"guardrail_name"`
	Params struct {
		Provider   string         `yaml:"guardrail" json:"guardrail"`
		Mode       string         `yaml:"mode" json:"mode"`
		APIBase    string         `yaml:"api_base" json:"api_base"`
		APIKey     string         `yaml:"api_key" json:"api_key"`
		Default    bool           `yaml:"default_on" json:"default_on"`
		Fallback   string         `yaml:"unreachable_fallback" json:"unreachable_fallback"`
		Parameters map[string]any `yaml:"additional_provider_specific_params" json:"additional_provider_specific_params"`
	} `yaml:"litellm_params" json:"litellm_params"`
}

func prepareGenericGuardrails(specs []genericGuardrailSpec) ([]runtime.GenericGuardrail, error) {
	result := make([]runtime.GenericGuardrail, 0, len(specs))
	seen := make(map[string]bool)
	for _, spec := range specs {
		p := spec.Params
		if spec.Name == "" || seen[spec.Name] || p.Provider != "generic_guardrail_api" || (p.Mode != "pre_call" && p.Mode != "post_call") ||
			(p.Fallback != "" && p.Fallback != "fail_open" && p.Fallback != "fail_closed") {
			return nil, errors.New("invalid or unsupported guardrail configuration")
		}
		base, err := resolveEnv(p.APIBase)
		if err != nil {
			return nil, err
		}
		if base == "" {
			base = os.Getenv("GENERIC_GUARDRAIL_API_BASE")
		}
		key, err := resolveEnv(p.APIKey)
		if err != nil {
			return nil, err
		}
		if key == "" {
			key = os.Getenv("GENERIC_GUARDRAIL_API_KEY")
		}
		target, err := url.Parse(base)
		if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
			target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.ForceQuery {
			return nil, errors.New("invalid generic guardrail API base")
		}
		const endpoint = "/beta/litellm_basic_guardrail_api"
		path := strings.TrimRight(target.Path, "/")
		if !strings.HasSuffix(path, endpoint) {
			path += endpoint
		}
		target.Path, target.RawPath = path, ""
		result = append(result, runtime.GenericGuardrail{Name: spec.Name, URL: target.String(), APIKey: key,
			Post: p.Mode == "post_call", Default: p.Default, FailOpen: p.Fallback == "fail_open", Parameters: p.Parameters})
		seen[spec.Name] = true
	}
	return result, nil
}
