package proxyconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

func ImportLiteLLMAgents(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	store.name = "agents"
	state := managedAgents{Public: slices.Clone(config.publicAgents)}
	if err := store.load(&state); err != nil {
		return 0, errors.New("could not read Go agent state")
	}
	if len(state.Entries) != 0 {
		return 0, errors.New("Go agent state already contains managed agents")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT to_jsonb(agent) FROM "LiteLLM_AgentsTable" AS agent ORDER BY agent_name,agent_id`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM_AgentsTable")
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, errors.New("could not read a LiteLLM agent")
		}
		var item struct {
			ID           string          `json:"agent_id"`
			Name         string          `json:"agent_name"`
			Params       json.RawMessage `json:"litellm_params"`
			Card         json.RawMessage `json:"agent_card_params"`
			Headers      json.RawMessage `json:"static_headers"`
			Extra        []string        `json:"extra_headers"`
			AccessGroups []string        `json:"agent_access_groups"`
			Permission   string          `json:"object_permission_id"`
			TPM          *int            `json:"tpm_limit"`
			RPM          *int            `json:"rpm_limit"`
			SessionTPM   *int            `json:"session_tpm_limit"`
			SessionRPM   *int            `json:"session_rpm_limit"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == "" || item.Name == "" {
			return 0, errors.New("invalid LiteLLM agent record")
		}
		if item.Permission != "" || item.TPM != nil || item.RPM != nil || item.SessionTPM != nil || item.SessionRPM != nil {
			return 0, fmt.Errorf("agent %q uses unsupported delegated permissions or rate limits", item.Name)
		}
		agent := agentConfig{ID: item.ID, Name: item.Name, ExtraHeaders: item.Extra, AccessGroups: item.AccessGroups}
		if decodeLiteLLMSearchField(item.Card, &agent.Card) != nil || decodeLiteLLMSearchField(item.Params, &agent.Parameters) != nil ||
			decodeLiteLLMSearchField(item.Headers, &agent.Headers) != nil {
			return 0, fmt.Errorf("agent %q has incompatible configuration", item.Name)
		}
		state.Entries = append(state.Entries, agent)
	}
	if rows.Err() != nil {
		return 0, errors.New("could not finish reading LiteLLM agents")
	}
	if len(state.Entries) == 0 {
		return 0, nil
	}
	if _, err := prepareAgents(append(slices.Clone(config.staticAgents), state.Entries...), state.Public); err != nil {
		return 0, err
	}
	if err := store.save(state); err != nil {
		return 0, errors.New("could not commit imported agent state")
	}
	return len(state.Entries), nil
}
