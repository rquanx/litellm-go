package proxyconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func mcpPublicID(name, resolvedURL string, entry mcpServerConfig) string {
	if entry.ServerID != "" {
		return entry.ServerID
	}
	fields := []string{name, resolvedURL, mcpPublicTransport(entry.Transport), entry.ImportedAuthType, entry.Alias}
	digest := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(digest[:16])
}

func mcpPublicTransport(transport string) string {
	if transport == "" || transport == "streamable_http" {
		return "http"
	}
	return transport
}

type mcpServerConfig struct {
	URL string `yaml:"url" json:"url"`
	Transport string `yaml:"transport" json:"transport"`
	Alias string `yaml:"alias" json:"alias,omitempty"`
	ServerID string `yaml:"server_id" json:"server_id"`
	Description string `yaml:"description" json:"description,omitempty"`
	AllowAllKeys bool `yaml:"allow_all_keys" json:"allow_all_keys"`
	Public bool `yaml:"available_on_public_internet" json:"available_on_public_internet,omitempty"`
	PublicInfo map[string]any `yaml:"mcp_info" json:"mcp_info,omitempty"`
	ImportedAuthType string `yaml:"-" json:"imported_auth_type,omitempty"`
	AuthType string `yaml:"auth_type" json:"auth_type,omitempty"`
	OAuthFlow string `yaml:"oauth2_flow" json:"oauth2_flow,omitempty"`
	AuthorizationURL string `yaml:"authorization_url" json:"authorization_url,omitempty"`
	OAuthCallbackURL string `yaml:"oauth_callback_url" json:"oauth_callback_url,omitempty"`
	TokenURL string `yaml:"token_url" json:"token_url,omitempty"`
	ClientID string `yaml:"client_id" json:"client_id,omitempty"`
	ClientSecret string `yaml:"client_secret" json:"client_secret,omitempty"`
	Scopes []string `yaml:"scopes" json:"scopes,omitempty"`
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
		publicID := mcpPublicID(name, base, entry)
		for _, value := range []string{name, entry.Alias, id, publicID} {
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
		oauth, err := entry.clientOAuth()
		if err != nil { return nil, fmt.Errorf("MCP server %q: %w", name, err) }
			interactive, err := entry.interactiveOAuth()
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", name, err)
		}
		result = append(result, runtime.MCPServer{ID: id, PublicID: publicID, Name: name, Alias: entry.Alias, URL: base, Transport: entry.Transport, OAuth: oauth, InteractiveOAuth: interactive,
			Command: entry.Command, Args: entry.Args, Env: env,
			Description: entry.Description, AllowAllKeys: entry.AllowAllKeys, AllowedTools: entry.AllowedTools,
			AccessGroups: append(slices.Clone(entry.AccessGroups), entry.YAMLAccessGroups...), Headers: headers,
			DisallowedTools: entry.DisallowedTools, AllowedParams: entry.AllowedParams})
	}
	return result, nil
}

func (entry mcpServerConfig) clientOAuth() (*runtime.MCPOAuth, error) {
	if entry.OAuthFlow == "authorization_code" || (entry.OAuthFlow == "" && entry.AuthorizationURL != "") {
		return nil, nil
	}
	if entry.OAuthFlow != "" && entry.OAuthFlow != "client_credentials" {
		return nil, fmt.Errorf("unsupported MCP OAuth flow")
	}
	if entry.TokenURL == "" && entry.ClientID == "" && entry.ClientSecret == "" {
		if entry.AuthType == "oauth2" {
			return nil, fmt.Errorf("OAuth client credentials require token_url, client_id and client_secret")
		}
		return nil, nil
	}
	if entry.Transport == "stdio" || (entry.AuthType != "" && entry.AuthType != "oauth2") {
		return nil, fmt.Errorf("unsupported OAuth transport or authentication mode")
	}
	tokenURL, urlErr := resolveEnv(entry.TokenURL)
	clientID, idErr := resolveEnv(entry.ClientID)
	secret, secretErr := resolveEnv(entry.ClientSecret)
	address, err := url.Parse(tokenURL)
	if urlErr != nil || idErr != nil || secretErr != nil || err != nil || clientID == "" || secret == "" ||
		address.Hostname() == "" || address.User != nil || address.Fragment != "" || (address.Scheme != "https" && address.Scheme != "http") {
		return nil, fmt.Errorf("invalid OAuth client credentials")
	}
	return &runtime.MCPOAuth{TokenURL: tokenURL, ClientID: clientID, ClientSecret: secret, Scopes: slices.Clone(entry.Scopes)}, nil
}

func (entry mcpServerConfig) interactiveOAuth() (*runtime.MCPInteractiveOAuth, error) {
	if entry.OAuthFlow != "authorization_code" && (entry.OAuthFlow != "" || entry.AuthorizationURL == "") {
		return nil, nil
	}
	if entry.Transport == "stdio" || (entry.AuthType != "" && entry.AuthType != "oauth2") {
		return nil, fmt.Errorf("unsupported interactive MCP authentication")
	}
	config := &runtime.MCPInteractiveOAuth{Scopes: slices.Clone(entry.Scopes)}
	for _, field := range []struct{value string; target *string}{
		{entry.AuthorizationURL, &config.AuthorizationURL},
		{entry.TokenURL, &config.TokenURL},
		{entry.OAuthCallbackURL, &config.CallbackURL},
		{entry.ClientID, &config.ClientID},
		{entry.ClientSecret, &config.ClientSecret},
	} {
		value, err := resolveEnv(field.value)
		if err != nil {
			return nil, err
		}
		*field.target = value
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}