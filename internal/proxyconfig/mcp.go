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
	URL string `yaml:"url"`
	Transport string `yaml:"transport"`
	Alias string `yaml:"alias"`
	ServerID string `yaml:"server_id"`
	Description string `yaml:"description"`
	AllowAllKeys bool `yaml:"allow_all_keys"`
	AllowedTools []string `yaml:"allowed_tools"`
	Headers map[string]string `yaml:"headers"`
}

func prepareMCPServers(entries map[string]mcpServerConfig) ([]runtime.MCPServer, error) {
	result := make([]runtime.MCPServer, 0, len(entries))
	identifiers := make(map[string]string)
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		entry := entries[name]
		if strings.TrimSpace(name) == "" || (entry.Transport != "" && entry.Transport != "http" && entry.Transport != "streamable_http") {
			return nil, fmt.Errorf("unsupported MCP server configuration for %q", name)
		}
		base, err := resolveEnv(entry.URL)
		if err != nil {
			return nil, err
		}
		address, err := url.Parse(base)
		if err != nil || address.Hostname() == "" || address.User != nil ||
			address.Fragment != "" || (address.Scheme != "http" && address.Scheme != "https") {
			return nil, fmt.Errorf("invalid URL for MCP server %q", name)
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
		headers := make(http.Header, len(entry.Headers))
		for key, value := range entry.Headers {
			resolved, err := resolveEnv(value)
			if err != nil {
				return nil, err
			}
			headers.Set(key, resolved)
		}
		result = append(result, runtime.MCPServer{ID: id, Name: name, Alias: entry.Alias, URL: address.String(),
			Description: entry.Description, AllowAllKeys: entry.AllowAllKeys, AllowedTools: entry.AllowedTools, Headers: headers})
	}
	return result, nil
}
