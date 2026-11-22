package proxyconfig

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type searchToolConfig struct {
	Name   string `yaml:"search_tool_name"`
	Params struct {
		Provider string  `yaml:"search_provider"`
		APIKey   string  `yaml:"api_key"`
		APIBase  string  `yaml:"api_base"`
		Timeout  float64 `yaml:"timeout"`
		Retries  *int    `yaml:"max_retries"`
	} `yaml:"litellm_params"`
	Info struct {
		Description string `yaml:"description"`
	} `yaml:"search_tool_info"`
}

func prepareSearchTools(entries []searchToolConfig) ([]runtime.SearchTool, error) {
	tools := make([]runtime.SearchTool, 0, len(entries))
	for _, entry := range entries {
		tool, err := entry.prepare()
		if err != nil {
			return nil, fmt.Errorf("search tool %q: %w", entry.Name, err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (entry searchToolConfig) prepare() (runtime.SearchTool, error) {
	params := entry.Params
	if strings.TrimSpace(entry.Name) == "" || strings.Contains(entry.Name, "/") ||
		params.Timeout < 0 || params.Timeout > 86400 || math.IsNaN(params.Timeout) ||
		(params.Retries != nil && (*params.Retries < 0 || *params.Retries > 8)) {
		return runtime.SearchTool{}, fmt.Errorf("invalid name, timeout or max_retries")
	}
	var defaultBase, baseEnv, keyEnv string
	switch params.Provider {
	case "perplexity":
		defaultBase, baseEnv, keyEnv = "https://api.perplexity.ai", "PERPLEXITY_API_BASE", "PERPLEXITYAI_API_KEY"
	case "tavily":
		defaultBase, baseEnv, keyEnv = "https://api.tavily.com", "TAVILY_API_BASE", "TAVILY_API_KEY"
	case "exa_ai":
		defaultBase, baseEnv, keyEnv = "https://api.exa.ai", "EXA_API_BASE", "EXA_API_KEY"
	case "linkup":
		defaultBase, baseEnv, keyEnv = "https://api.linkup.so/v1", "LINKUP_API_BASE", "LINKUP_API_KEY"
	case "firecrawl":
		defaultBase, baseEnv, keyEnv = "https://api.firecrawl.dev/v2", "FIRECRAWL_API_BASE", "FIRECRAWL_API_KEY"
	default:
		return runtime.SearchTool{}, fmt.Errorf("unsupported search_provider %q", params.Provider)
	}
	base, err := resolveEnv(params.APIBase)
	if err != nil {
		return runtime.SearchTool{}, err
	}
	if base == "" {
		base = os.Getenv(baseEnv)
	}
	if base == "" {
		base = defaultBase
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.Fragment != "" {
		return runtime.SearchTool{}, fmt.Errorf("invalid search API base")
	}
	if !strings.HasSuffix(strings.TrimRight(target.Path, "/"), "/search") {
		target = target.JoinPath("search")
	}
	key, err := resolveEnv(params.APIKey)
	if err != nil {
		return runtime.SearchTool{}, err
	}
	if key == "" {
		key = os.Getenv(keyEnv)
	}
	if key == "" {
		return runtime.SearchTool{}, fmt.Errorf("missing search API key (%s)", keyEnv)
	}
	return runtime.SearchTool{Name: entry.Name, Provider: params.Provider, URL: target.String(), APIKey: key,
		Description: entry.Info.Description, Timeout: time.Duration(params.Timeout * float64(time.Second)), Retries: params.Retries}, nil
}
