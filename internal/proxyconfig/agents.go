package proxyconfig

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

type agentConfig struct {
	ID           string            `yaml:"-" json:"agent_id,omitempty"`
	Name         string            `yaml:"agent_name" json:"agent_name"`
	Card         map[string]any    `yaml:"agent_card_params" json:"agent_card_params"`
	Parameters   map[string]any    `yaml:"litellm_params" json:"litellm_params,omitempty"`
	Headers      map[string]string `yaml:"static_headers" json:"static_headers,omitempty"`
	AccessGroups []string          `yaml:"agent_access_groups" json:"agent_access_groups,omitempty"`
	ExtraHeaders []string          `yaml:"extra_headers" json:"extra_headers,omitempty"`
}

func prepareAgents(config []agentConfig, public []string) ([]runtime.Agent, error) {
	agents := make([]runtime.Agent, 0, len(config))
	names := make(map[string]bool)
	for _, entry := range config {
		if entry.Name == "" || strings.ContainsAny(entry.Name, "/\\") || names[entry.Name] {
			return nil, fmt.Errorf("invalid or duplicate agent_name %q", entry.Name)
		}
		names[entry.Name] = true
		raw, _ := entry.Card["url"].(string)
		resolved, err := resolveEnv(raw)
		if err != nil {
			return nil, err
		}
		address, err := url.Parse(resolved)
		if err != nil || address.Hostname() == "" || address.User != nil || address.Fragment != "" ||
			(address.Scheme != "http" && address.Scheme != "https") {
			return nil, fmt.Errorf("invalid A2A URL for agent %q", entry.Name)
		}
		if provider, _ := entry.Parameters["custom_llm_provider"].(string); provider != "" && provider != "a2a_agent" {
			return nil, fmt.Errorf("unsupported provider bridge for agent %q", entry.Name)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid card for agent %q", entry.Name)
		}
		id := entry.ID
		if id == "" {
			id = fmt.Sprintf("%x", sha256.Sum256(encoded))
		}
		headers := make(http.Header)
		for name, value := range entry.Headers {
			resolved, err := resolveEnv(value)
			if err != nil {
				return nil, err
			}
			headers.Set(name, resolved)
		}
		agents = append(agents, runtime.Agent{ID: id, Name: entry.Name, URL: address.String(), Card: entry.Card, Headers: headers,
			ExtraHeaders: entry.ExtraHeaders, AccessGroups: slices.Clone(entry.AccessGroups), Public: slices.Contains(public, id) || slices.Contains(public, entry.Name)})
	}
	return agents, nil
}

func (registry *KeyRegistry) AuthorizeAgent(token string) func(runtime.Agent) bool {
	_, permissions, valid := registry.authorize(token)
	if !valid {
		return nil
	}
	scopes := []*objectPermission{permissions.key, permissions.team, permissions.project}
	return func(agent runtime.Agent) bool {
		restricted := false
		for _, permission := range scopes {
			if permission == nil || len(permission.Agents)+len(permission.AgentGroups) == 0 {
				continue
			}
			restricted = true
			if !slices.Contains(permission.Agents, agent.ID) &&
				!slices.Contains(permission.Agents, agent.Name) &&
				!slices.ContainsFunc(agent.AccessGroups, func(group string) bool { return slices.Contains(permission.AgentGroups, group) }) {
				return false
			}
		}
		return restricted || agent.Public
	}
}
