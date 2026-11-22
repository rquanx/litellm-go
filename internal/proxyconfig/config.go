package proxyconfig

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
	"go.yaml.in/yaml/v3"
)

type configFile struct {
	Models  []deployment `yaml:"model_list"`
	General struct {
		MasterKey string `yaml:"master_key"`
	} `yaml:"general_settings"`
	Router struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
		Timeout   int                  `yaml:"timeout"`
		Retries   int                  `yaml:"num_retries"`
	} `yaml:"router_settings"`
}

type deployment struct {
	Alias  string `yaml:"model_name"`
	Params struct {
		Model   string `yaml:"model"`
		APIBase string `yaml:"api_base"`
		APIKey  string `yaml:"api_key"`
	} `yaml:"litellm_params"`
	Info struct {
		Endpoints []string `yaml:"supported_endpoints"`
	} `yaml:"model_info"`
}

type Config struct {
	Routes    []runtime.Route
	MasterKey string
	Timeout   time.Duration
}

func Load(reader io.Reader) (Config, error) {
	var file configFile
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return Config{}, errors.New("invalid or unsupported LiteLLM YAML configuration")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("expected one LiteLLM YAML document")
	}
	if len(file.Models) == 0 || file.Router.Timeout < 0 || file.Router.Timeout > 86400 || file.Router.Retries != 0 {
		return Config{}, errors.New("model_list is required and router settings must use a valid timeout and zero retries")
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second}
	var err error
	config.MasterKey, err = resolveEnv(file.General.MasterKey)
	if err != nil {
		return Config{}, fmt.Errorf("general_settings.master_key: %w", err)
	}
	byAlias := make(map[string][]runtime.Candidate)
	var aliases []string
	for _, model := range file.Models {
		candidate, err := model.candidate()
		if err != nil {
			return Config{}, fmt.Errorf("model %q: %w", model.Alias, err)
		}
		if _, exists := byAlias[model.Alias]; !exists {
			aliases = append(aliases, model.Alias)
		}
		byAlias[model.Alias] = append(byAlias[model.Alias], candidate)
	}
	fallbacks := make(map[string][]string)
	for _, entry := range file.Router.Fallbacks {
		for alias, targets := range entry {
			_, exists := fallbacks[alias]
			if len(byAlias[alias]) == 0 || exists {
				return Config{}, fmt.Errorf("unknown or repeated fallback alias %q", alias)
			}
			fallbacks[alias] = targets
		}
	}
	for _, alias := range aliases {
		candidates := append([]runtime.Candidate(nil), byAlias[alias]...)
		seen := map[string]bool{alias: true}
		for _, target := range fallbacks[alias] {
			if seen[target] || len(byAlias[target]) == 0 {
				return Config{}, fmt.Errorf("model %q has an unknown or repeated fallback %q", alias, target)
			}
			seen[target] = true
			candidates = append(candidates, byAlias[target]...)
		}
		if len(candidates) > 16 {
			return Config{}, fmt.Errorf("model %q exceeds the 16-candidate limit", alias)
		}
		embedding := candidates[0].Endpoint == "embeddings"
		for _, candidate := range candidates {
			if (candidate.Endpoint == "embeddings") != embedding {
				return Config{}, fmt.Errorf("model %q mixes embeddings with text generation", alias)
			}
		}
		config.Routes = append(config.Routes, runtime.Route{Alias: alias, Version: "1", Candidates: candidates})
	}
	return config, nil
}

func (entry deployment) candidate() (runtime.Candidate, error) {
	model, compatible := strings.CutPrefix(entry.Params.Model, "openai/")
	if entry.Alias == "" || model == "" || !compatible {
		return runtime.Candidate{}, errors.New("model_name and an openai/ model are required")
	}
	model, _ = strings.CutPrefix(model, "chat_completions/")
	if model == "" {
		return runtime.Candidate{}, errors.New("model ID is required")
	}
	base, err := resolveEnv(entry.Params.APIBase)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_base: %w", err)
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return runtime.Candidate{}, errors.New("api_base must be an HTTP or HTTPS URL without credentials, query or fragment")
	}
	key, err := resolveEnv(entry.Params.APIKey)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_key: %w", err)
	}
	endpoint := "chat-completions"
	if len(entry.Info.Endpoints) > 1 {
		return runtime.Candidate{}, errors.New("each deployment accepts one supported_endpoints entry")
	}
	if len(entry.Info.Endpoints) == 1 {
		switch entry.Info.Endpoints[0] {
		case "/v1/responses":
			endpoint = "responses"
		case "/v1/embeddings":
			endpoint = "embeddings"
		case "/v1/chat/completions":
		default:
			return runtime.Candidate{}, errors.New("unsupported model endpoint")
		}
	}
	return runtime.Candidate{SourceID: "openai", ModelID: model, APIBase: base, APIKey: key, Endpoint: endpoint}, nil
}

func resolveEnv(value string) (string, error) {
	name, referenced := strings.CutPrefix(value, "os.environ/")
	if !referenced {
		return value, nil
	}
	resolved := os.Getenv(name)
	if resolved == "" {
		return "", fmt.Errorf("environment variable %q is missing", name)
	}
	return resolved, nil
}
