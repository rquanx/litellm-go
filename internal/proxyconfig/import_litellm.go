package proxyconfig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
)

// ImportLiteLLMModels copies supported LiteLLM deployments into empty Go proxy state.
func ImportLiteLLMModels(db *sql.DB, config Config) (int, error) {
	store := postgresModels(db, config.MasterKey)
	var existing []deployment
	if err := store.load(&existing); err != nil {
		return 0, errors.New("could not read Go model state")
	}
	if len(existing) != 0 {
		return 0, errors.New("Go model state already contains deployments")
	}
	salt := os.Getenv("LITELLM_SALT_KEY")
	if salt == "" {
		salt = config.MasterKey
	}
	if salt == "" {
		return 0, errors.New("the source LiteLLM salt or master key is required")
	}
	models, err := readLiteLLMModels(db, salt)
	if err != nil {
		return 0, err
	}
	if len(models) == 0 {
		return 0, nil
	}
	registry := modelRegistry{base: config}
	if _, err := registry.compile(models); err != nil {
		return 0, err
	}
	if err := store.save(models); err != nil {
		return 0, errors.New("could not commit imported model state")
	}
	return len(models), nil
}

func ImportLiteLLMKeys(db *sql.DB) (int, error) {
	store := &postgresState{db: db, name: "keys"}
	var existing map[string]virtualKey
	if err := store.load(&existing); err != nil {
		return 0, errors.New("could not read Go key state")
	}
	if len(existing) != 0 {
		return 0, errors.New("Go key state already contains credentials")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT to_jsonb(key_row) FROM "LiteLLM_VerificationToken" AS key_row`)
	if err != nil {
		return 0, errors.New("could not read LiteLLM_VerificationToken")
	}
	defer rows.Close()
	keys := make(map[string]virtualKey)
	for rows.Next() {
		var payload []byte
		if rows.Scan(&payload) != nil {
			return 0, errors.New("could not read a LiteLLM virtual key")
		}
		token, key, blocked, err := decodeLiteLLMKey(payload)
		if err != nil {
			return 0, err
		}
		if !blocked {
			keys[token] = key
		}
	}
	if rows.Err() != nil {
		return 0, errors.New("could not finish reading LiteLLM virtual keys")
	}
	if len(keys) == 0 {
		return 0, nil
	}
	if err := store.save(keys); err != nil {
		return 0, errors.New("could not commit imported virtual keys")
	}
	return len(keys), nil
}

func decodeLiteLLMKey(payload []byte) (string, virtualKey, bool, error) {
	var key virtualKey
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return "", key, false, errors.New("invalid LiteLLM virtual key")
	}
	if string(fields["blocked"]) == "true" {
		return "", key, true, nil
	}
	expires := fields["expires"]
	delete(fields, "expires")
	keyFields, _ := json.Marshal(fields)
	if json.Unmarshal(keyFields, &key) != nil {
		return "", key, false, errors.New("invalid LiteLLM virtual key")
	}
	if len(expires) != 0 && string(expires) != "null" {
		var date string
		if json.Unmarshal(expires, &date) != nil {
			return "", key, false, errors.New("invalid LiteLLM key expiration")
		}
		instant, err := time.Parse(time.RFC3339Nano, date)
		if err != nil {
			instant, err = time.Parse("2006-01-02T15:04:05.999999999", date)
		}
		if err != nil {
			return "", key, false, errors.New("invalid LiteLLM key expiration")
		}
		key.Expires = &instant
	}
	for _, field := range []string{"team_id", "user_id", "agent_id", "project_id", "organization_id", "budget_id",
		"max_budget", "max_parallel_requests", "tpm_limit", "rpm_limit", "object_permission_id", "allowed_routes",
		"allowed_cache_controls", "access_group_ids", "permissions", "policies", "router_settings", "config", "aliases",
		"model_max_budget", "budget_limits", "auto_rotate"} {
		switch string(fields[field]) {
		case "", "null", "[]", "{}", "\"\"", "false":
		default:
			return "", key, false, fmt.Errorf("virtual key %q uses unsupported field %s", key.Alias, field)
		}
	}
	for _, alias := range []string{"all-team-models", "all-proxy-models"} {
		if slices.Contains(key.Models, alias) {
			return "", key, false, fmt.Errorf("virtual key %q uses a dynamic model scope", key.Alias)
		}
	}
	var token string
	if json.Unmarshal(fields["token"], &token) != nil {
		return "", key, false, errors.New("LiteLLM virtual key is missing its digest")
	}
	digest, err := hex.DecodeString(token)
	if err != nil || len(digest) != sha256.Size {
		return "", key, false, errors.New("LiteLLM virtual key uses an unsupported digest")
	}
	return hex.EncodeToString(digest), key, false, nil
}

func readLiteLLMModels(db *sql.DB, salt string) ([]deployment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT model_id, model_name, litellm_params, COALESCE(model_info, '{}'::jsonb)
		FROM "LiteLLM_ProxyModelTable" ORDER BY created_at, model_id`)
	if err != nil {
		return nil, errors.New("could not read LiteLLM_ProxyModelTable")
	}
	defer rows.Close()
	var models []deployment
	for rows.Next() {
		var model deployment
		var id string
		var params, info []byte
		if err := rows.Scan(&id, &model.Alias, &params, &info); err != nil {
			return nil, errors.New("could not read a LiteLLM deployment")
		}
		params, err = decryptLiteLLMParams(params, salt)
		if err != nil || decodeModelJSON(bytes.NewReader(params), &model.Params) != nil ||
			json.Unmarshal(info, &model.Info) != nil {
			return nil, fmt.Errorf("model %q has incompatible configuration or credentials", model.Alias)
		}
		model.Info.ID = id
		models = append(models, model)
	}
	if rows.Err() != nil {
		return nil, errors.New("could not finish reading LiteLLM models")
	}
	return models, nil
}

func decryptLiteLLMParams(payload []byte, salt string) ([]byte, error) {
	var params map[string]any
	if err := json.Unmarshal(payload, &params); err != nil || params == nil {
		return nil, errors.New("invalid LiteLLM model parameters")
	}
	digest := sha256.Sum256([]byte(salt))
	for name, value := range params {
		encoded, ok := value.(string)
		if !ok || encoded == "" {
			continue
		}
		data, err := base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			data, err = base64.StdEncoding.DecodeString(encoded)
		}
		if err != nil || len(data) < 24+secretbox.Overhead {
			continue
		}
		var nonce [24]byte
		copy(nonce[:], data[:24])
		plain, valid := secretbox.Open(nil, data[24:], &nonce, &digest)
		if !valid {
			return nil, errors.New("invalid LiteLLM database salt")
		}
		params[name] = string(plain)
	}
	return json.Marshal(params)
}
