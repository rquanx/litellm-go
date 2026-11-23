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
		Provider           string         `yaml:"guardrail" json:"guardrail"`
		Mode               string         `yaml:"mode" json:"mode"`
		APIBase            string         `yaml:"api_base" json:"api_base"`
		APIKey             string         `yaml:"api_key" json:"api_key"`
		Default            bool           `yaml:"default_on" json:"default_on"`
		Fallback           string         `yaml:"unreachable_fallback" json:"unreachable_fallback"`
		Parameters         map[string]any `yaml:"additional_provider_specific_params" json:"additional_provider_specific_params"`
		APIVersion         string         `yaml:"api_version" json:"api_version"`
		Severity           int            `yaml:"severity_threshold" json:"severity_threshold"`
		SeverityByCategory map[string]int `yaml:"severity_threshold_by_category" json:"severity_threshold_by_category"`
		Categories         []string       `yaml:"categories" json:"categories"`
		Blocklists         []string       `yaml:"blocklistNames" json:"blocklistNames"`
		HaltOnBlocklistHit bool           `yaml:"haltOnBlocklistHit" json:"haltOnBlocklistHit"`
		OutputType         string         `yaml:"outputType" json:"outputType"`
	} `yaml:"litellm_params" json:"litellm_params"`
}

func supportedGuardrailProvider(provider string) bool {
	switch provider {
	case "generic_guardrail_api", "azure/prompt_shield", "azure/text_moderations":
		return true
	default:
		return false
	}
}

func prepareGenericGuardrails(specs []genericGuardrailSpec) ([]runtime.GenericGuardrail, error) {
	result := make([]runtime.GenericGuardrail, 0, len(specs))
	seen := make(map[string]bool)
	for _, spec := range specs {
		p := spec.Params
		if spec.Name == "" || seen[spec.Name] || !supportedGuardrailProvider(p.Provider) || (p.Mode != "pre_call" && p.Mode != "post_call") ||
			(p.Provider == "azure/prompt_shield" && p.Mode != "pre_call") ||
			(p.Fallback != "" && p.Fallback != "fail_open" && p.Fallback != "fail_closed") {
			return nil, errors.New("invalid or unsupported guardrail configuration")
		}
		base, err := resolveEnv(p.APIBase)
		if err != nil {
			return nil, err
		}
		if base == "" && p.Provider == "generic_guardrail_api" {
			base = os.Getenv("GENERIC_GUARDRAIL_API_BASE")
		}
		key, err := resolveEnv(p.APIKey)
		if err != nil {
			return nil, err
		}
		if key == "" && p.Provider == "generic_guardrail_api" {
			key = os.Getenv("GENERIC_GUARDRAIL_API_KEY")
		}
		target, err := url.Parse(base)
		if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
			target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.ForceQuery {
			return nil, errors.New("invalid guardrail API base")
		}
		endpoint := "/beta/litellm_basic_guardrail_api"
		var azure *runtime.AzureSafetyOptions
		if p.Provider != "generic_guardrail_api" {
			if key == "" {
				return nil, errors.New("Azure Content Safety requires an API key")
			}
			endpoint = "/contentsafety/text:analyze"
			if p.Provider == "azure/prompt_shield" {
				endpoint = "/contentsafety/text:shieldPrompt"
			}
			if p.APIVersion == "" {
				p.APIVersion = "2024-09-01"
			}
			target.RawQuery = url.Values{"api-version": {p.APIVersion}}.Encode()
			if len(p.Categories) == 0 {
				p.Categories = []string{"Hate", "Sexual", "SelfHarm", "Violence"}
			}
			if p.OutputType == "" {
				p.OutputType = "FourSeverityLevels"
			}
			azure = &runtime.AzureSafetyOptions{Severity: p.Severity, SeverityByCategory: p.SeverityByCategory, Categories: p.Categories,
				Blocklists: p.Blocklists, HaltOnBlocklistHit: p.HaltOnBlocklistHit, OutputType: p.OutputType}
		}
		path := strings.TrimRight(target.Path, "/")
		if !strings.HasSuffix(path, endpoint) {
			path += endpoint
		}
		target.Path, target.RawPath = path, ""
		result = append(result, runtime.GenericGuardrail{Name: spec.Name, URL: target.String(), APIKey: key,
			Post: p.Mode == "post_call", Default: p.Default, FailOpen: p.Fallback == "fail_open", Parameters: p.Parameters, Provider: p.Provider, Azure: azure})
		seen[spec.Name] = true
	}
	return result, nil
}
