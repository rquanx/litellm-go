package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func ImportLiteLLMMCPServers(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	var state modelState
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go proxy state")
	}
	if len(state.MCPServers) != 0 {
		return 0, errors.New("Go proxy state already contains managed MCP servers")
	}
	if len(state.MCPToolsets) != 0 {
		return 0, errors.New("Go proxy state already contains MCP toolsets")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT to_jsonb(s) FROM "LiteLLM_MCPServerTable" s ORDER BY server_id`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM_MCPServerTable")
	}
	defer rows.Close()
	salt := os.Getenv("LITELLM_SALT_KEY")
	if salt == "" {
		salt = config.MasterKey
	}
	state.MCPServers = make(map[string]mcpServerConfig)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return 0, errors.New("could not read a LiteLLM MCP server")
		}
		name, entry, inactive, err := decodeLiteLLMMCPServer(payload, salt)
		if err != nil {
			return 0, err
		}
		if inactive {
			continue
		}
		if _, exists := state.MCPServers[name]; exists {
			return 0, fmt.Errorf("duplicate LiteLLM MCP server name %q", name)
		}
		state.MCPServers[name] = entry
	}
	if err := rows.Err(); err != nil {
		return 0, errors.New("could not finish reading LiteLLM MCP servers")
	}
	toolsets, err := readLiteLLMMCPToolsets(ctx, db)
	if err != nil {
		return 0, err
	}
	state.MCPToolsets = toolsets
	if len(state.MCPServers)+len(state.MCPToolsets) == 0 {
		return 0, nil
	}
	registry := modelRegistry{base: config}
	if _, err := registry.compileMCP(state.MCPServers); err != nil {
		return 0, err
	}
	if err := store.save(state); err != nil {
		return 0, errors.New("could not commit imported MCP server state")
	}
	return len(state.MCPServers), nil
}

func readLiteLLMMCPToolsets(ctx context.Context, db *sql.DB) ([]runtime.MCPToolset, error) {
	var present bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('"LiteLLM_MCPToolsetTable"') IS NOT NULL`).Scan(&present); err != nil || !present {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT toolset_id, toolset_name, COALESCE(description, ''), tools, created_at, updated_at
		FROM "LiteLLM_MCPToolsetTable" ORDER BY created_at, toolset_id`)
	if err != nil {
		return nil, errors.New("could not read LiteLLM_MCPToolsetTable")
	}
	defer rows.Close()
	var result []runtime.MCPToolset
	for rows.Next() {
		var set runtime.MCPToolset
		var tools []byte
		if err := rows.Scan(&set.ID, &set.Name, &set.Description, &tools, &set.CreatedAt, &set.UpdatedAt); err != nil {
			return nil, errors.New("invalid LiteLLM MCP toolset")
		}
		if decodeLiteLLMSearchField(tools, &set.Tools) != nil || set.ID == "" || set.Name == "" || strings.Contains(set.Name, "/") {
			return nil, fmt.Errorf("incompatible MCP toolset %q", set.Name)
		}
		for _, tool := range set.Tools {
			if tool.ServerID == "" || tool.Name == "" {
				return nil, fmt.Errorf("incompatible tool entry in MCP toolset %q", set.Name)
			}
		}
		result = append(result, set)
	}
	return result, rows.Err()
}

func decodeLiteLLMMCPServer(payload []byte, salt string) (string, mcpServerConfig, bool, error) {
	var row struct {
		mcpServerInput
		ApprovalStatus string `json:"approval_status"`
		AuthType string `json:"auth_type"`
		SpecPath string `json:"spec_path"`
		DelegateAuth bool `json:"delegate_auth_to_upstream"`
		BYOK bool `json:"is_byok"`
		ExtraHeaders []string `json:"extra_headers"`
		Credentials json.RawMessage `json:"credentials"`
		Headers json.RawMessage `json:"static_headers"`
	}
	if json.Unmarshal(payload, &row) != nil {
		return "", mcpServerConfig{}, false, errors.New("invalid LiteLLM MCP server row")
	}
	if row.ApprovalStatus != "" && row.ApprovalStatus != "active" && row.ApprovalStatus != "approved" {
		return "", mcpServerConfig{}, true, nil
	}
	entry := row.mcpServerConfig
	entry.ImportedAuthType = row.AuthType
	name := row.Name
	if name == "" {
		name = entry.Alias
	}
	if name == "" {
		name = entry.ServerID
	}
	if row.SpecPath != "" || row.DelegateAuth || row.BYOK || len(row.ExtraHeaders) != 0 {
		return "", entry, false, fmt.Errorf("MCP server %q uses unsupported routing or credential settings", name)
	}
	if decodeLiteLLMSearchField(row.Headers, &entry.Headers) != nil {
		return "", entry, false, fmt.Errorf("MCP server %q has invalid static headers", name)
	}
	if entry.Headers == nil {
		entry.Headers = make(map[string]string)
	}
	var credentials map[string]string
	if decodeLiteLLMSearchField(row.Credentials, &credentials) != nil {
		return "", entry, false, fmt.Errorf("MCP server %q has incompatible credentials", name)
	}
	switch row.AuthType {
	case "", "none":
		if len(credentials) != 0 {
			return "", entry, false, fmt.Errorf("MCP server %q has credentials without a supported auth type", name)
		}
	case "authorization", "bearer_token", "api_key", "basic", "token":
		if len(credentials) != 1 || credentials["auth_value"] == "" {
			return "", entry, false, fmt.Errorf("MCP server %q requires a static auth value", name)
		}
		encoded, _ := json.Marshal(credentials)
		decoded, err := decryptLiteLLMParams(encoded, salt)
		if err != nil || json.Unmarshal(decoded, &credentials) != nil {
			return "", entry, false, fmt.Errorf("MCP server %q credentials could not be decrypted", name)
		}
		value := credentials["auth_value"]
		switch row.AuthType {
		case "bearer_token":
			entry.Headers["Authorization"] = "Bearer " + value
		case "api_key":
			entry.Headers["X-API-Key"] = value
		case "basic":
			if strings.Contains(value, ":") {
				value = base64.StdEncoding.EncodeToString([]byte(value))
			}
			entry.Headers["Authorization"] = "Basic " + value
		case "token":
			entry.Headers["Authorization"] = "token " + value
		default:
			entry.Headers["Authorization"] = value
		}
	default:
		return "", entry, false, fmt.Errorf("MCP server %q uses unsupported authentication", name)
	}
	return name, entry, false, nil
}
