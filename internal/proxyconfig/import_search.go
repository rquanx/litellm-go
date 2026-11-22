package proxyconfig

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ImportLiteLLMSearchTools preserves search tool IDs and publishes a validated state snapshot.
func ImportLiteLLMSearchTools(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	var state modelState
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go proxy state")
	}
	if len(state.SearchTools) != 0 {
		return 0, errors.New("Go proxy state already contains managed search tools")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT search_tool_id, search_tool_name, litellm_params, search_tool_info, created_at, updated_at
		FROM "LiteLLM_SearchToolsTable" ORDER BY created_at, search_tool_id`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM_SearchToolsTable")
	}
	defer rows.Close()
	for rows.Next() {
		var entry managedSearchTool
		var params, info []byte
		if err := rows.Scan(&entry.ID, &entry.Name, &params, &info, &entry.CreatedAt, &entry.UpdatedAt); err != nil {
			return 0, errors.New("could not read a LiteLLM search tool")
		}
		if decodeLiteLLMSearchField(params, &entry.Params) != nil || decodeLiteLLMSearchField(info, &entry.Info) != nil {
			return 0, fmt.Errorf("search tool %q has incompatible configuration", entry.Name)
		}
		state.SearchTools = append(state.SearchTools, entry)
	}
	if rows.Err() != nil {
		return 0, errors.New("could not finish reading LiteLLM search tools")
	}
	if len(state.SearchTools) == 0 {
		return 0, nil
	}
	registry := modelRegistry{base: config, credentials: state.Credentials, router: state.Router, searchTools: state.SearchTools}
	if _, err := registry.compile(state.Models); err != nil {
		return 0, err
	}
	if err := store.save(state); err != nil {
		return 0, errors.New("could not commit imported search tools")
	}
	return len(state.SearchTools), nil
}

func decodeLiteLLMSearchField(raw []byte, target any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return err
		}
		raw = []byte(encoded)
	}
	return decodeModelJSON(bytes.NewReader(raw), target)
}
