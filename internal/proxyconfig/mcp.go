package proxyconfig

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type mcpServerConfig struct {
	URL string `yaml:"url" json:"url"`
	Transport string `yaml:"transport" json:"transport"`
	Alias string `yaml:"alias" json:"alias,omitempty"`
	ServerID string `yaml:"server_id" json:"server_id"`
	Description string `yaml:"description" json:"description,omitempty"`
	AllowAllKeys bool `yaml:"allow_all_keys" json:"allow_all_keys"`
	AllowedTools []string `yaml:"allowed_tools" json:"allowed_tools"`
	AccessGroups []string `yaml:"mcp_access_groups" json:"mcp_access_groups,omitempty"`
	YAMLAccessGroups []string `yaml:"access_groups" json:"-"`
	StaticHeaders map[string]string `yaml:"static_headers" json:"-"`
	DisallowedTools []string `yaml:"disallowed_tools" json:"disallowed_tools,omitempty"`
	AllowedParams map[string][]string `yaml:"allowed_params" json:"allowed_params,omitempty"`
	Headers map[string]string `yaml:"headers" json:"static_headers,omitempty"`
	Command string `yaml:"command" json:"command,omitempty"`
	Args []string `yaml:"args" json:"args,omitempty"`
	Env map[string]string `yaml:"env" json:"env,omitempty"`
}

func prepareMCPServers(entries map[string]mcpServerConfig) ([]runtime.MCPServer, error) {
	result := make([]runtime.MCPServer, 0, len(entries))
	identifiers := make(map[string]string)
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		if strings.TrimSpace(name) == "" || !slices.Contains([]string{"", "http", "streamable_http", "stdio", "sse"}, entry.Transport) {
			return nil, fmt.Errorf("unsupported MCP server configuration for %q", name)
		}
		base, err := resolveEnv(entry.URL)
		if err != nil {
			return nil, err
		}
		if entry.Transport == "stdio" {
			if strings.TrimSpace(entry.Command) == "" || base != "" {
				return nil, fmt.Errorf("stdio MCP server %q requires a command", name)
			}
		} else {
			address, err := url.Parse(base)
			if entry.Command != "" || err != nil || address.Hostname() == "" || address.User != nil ||
				address.Fragment != "" || (address.Scheme != "http" && address.Scheme != "https") {
				return nil, fmt.Errorf("invalid URL for MCP server %q", name)
			}
			base = address.String()
		}
		id := entry.ServerID
		if id == "" {
			id = name
		}
		for _, value := range []string{name, entry.Alias, id} {
			if value == "" {
				continue
			}
			if previous, exists := identifiers[value]; exists && previous != name {
				return nil, fmt.Errorf("duplicate MCP server identifier %q", value)
			}
			identifiers[value] = name
		}
		configuredHeaders := maps.Clone(entry.Headers)
		if configuredHeaders == nil {
			configuredHeaders = make(map[string]string)
		}
		maps.Copy(configuredHeaders, entry.StaticHeaders)
		headers := make(http.Header, len(configuredHeaders))
		for key, value := range configuredHeaders {
			resolved, err := resolveEnv(value)
			if err != nil {
				return nil, err
			}
			headers.Set(key, resolved)
		}
		var env []string
		for _, key := range slices.Sorted(maps.Keys(entry.Env)) {
			value, err := resolveEnv(entry.Env[key])
			if err != nil {
				return nil, err
			}
			env = append(env, key+"="+value)
		}
		result = append(result, runtime.MCPServer{ID: id, Name: name, Alias: entry.Alias, URL: base, Transport: entry.Transport,
			Command: entry.Command, Args: entry.Args, Env: env,
			Description: entry.Description, AllowAllKeys: entry.AllowAllKeys, AllowedTools: entry.AllowedTools,
			AccessGroups: append(slices.Clone(entry.AccessGroups), entry.YAMLAccessGroups...), Headers: headers,
			DisallowedTools: entry.DisallowedTools, AllowedParams: entry.AllowedParams})
	}
	return result, nil
}
